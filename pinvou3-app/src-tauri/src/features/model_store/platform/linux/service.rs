//! 模型商店的服务层：把阻塞的生命周期操作包装成「立刻返回 run_id + 长轮询日志」。
//!
//! 这个文件顶替了 LMM 的 `core/admin.rs`（上游 919 行）。那一版是本地 HTTP 管理接口，
//! 为 sidecar 形态存在——宿主进程和 LMM 是两个进程，只能靠 HTTP 说话，于是要
//! token 鉴权、要 JSON 序列化、要"token 永不出栈到前端"的安全约束。
//!
//! 原生实现之后这些统统不需要了：同一个进程，命令层直接调这里的方法。**但
//! `admin.rs` 里真正值钱的东西不是 HTTP，而是长操作的骨架**，那部分原样搬了过来：
//!
//! - `spawn_op` 的 Drop 守卫 + `catch_unwind`（job 一 panic，卡片会被永久锁死、
//!   长轮询永远不返回——真机上出过）
//! - `OpCleanup::drop` 里 `finish` 必须是最后一步（它一置位长轮询立刻返回，
//!   调用方下一毫秒就可能重试同一张卡，那时互斥必须已经放开）
//! - `RunLog` 把行存下来而不是只往外推（调用方晚到时不丢前半段日志，而前半段
//!   往往正是出错的地方）
//! - 列表接口必须摘掉 `scripts_content` / `assets_content`（五张卡就能把响应
//!   撑到 8.6 MB，而列表页一个字都用不到）

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use parking_lot::Mutex;
use serde_json::{Value, json};

use super::lifecycle::ModelManager;
use super::run_log_store::{self, RunLogFile};
use super::run_state::{
    ActiveCardOps, CancelRegistry, CardOp, DownloadQueue, OP_DEPLOY, OP_DOWNLOAD, OP_ENABLE,
    OP_FORCE_REMOVE, OP_INSTALL_LOCAL, OP_STOP, OP_UNINSTALL, OP_UNINSTALL_RECOVERY,
    StartupCleanup,
};
use super::{config, db, disk, import_scan, script_runner};

/// 运行记录保留多久。一次下载的日志能有几万行，不清会一直涨。
///
/// 到点之后**内存里**这条运行整个消失（`sweep_old_runs`）。日志本身不会跟着没——
/// 它已经由 `run_log_store` 落在 `~/.pinvou3/model-store/logs/` 下了，这正是加那个
/// 模块的原因：在它之前，600 秒一过「昨天那次下载为什么失败」在设备上无法回答。
/// （megacube 整合把保留期从 600 秒放宽到 7 天：远端商店托管的运行要跨天接回。）
const RUN_RETENTION: Duration = Duration::from_secs(7 * 24 * 3600);
/// 长轮询最多挂多久。跟前端的 `LOG_WAIT_SECONDS` 对齐。
const LOG_TAIL_BYTES: usize = 1024 * 1024;
const LOG_TAIL_LINES: usize = 4096;
const LOG_WAIT: Duration = Duration::from_secs(25);

/// 这一行是不是脚本在请求用户输入。
///
/// 判据跟 `script_runner` 里那份一致——它的 `is_prompt_line` 是私有的，对外的
/// 约定是那两个前缀常量。这里只为一件事要它：记住「现在是不是有一条提示在等人
/// 答」，见 [`RunLog::push`]。
fn is_prompt_line(line: &str) -> bool {
    let t = line.trim();
    t.starts_with(script_runner::PROMPT_MARKER) || t.starts_with(script_runner::PROMPT_FORM_MARKER)
}

/// Docker pull 会为每一层反复重绘同一行，例如
/// `[安装引擎] a1b2c3d4: Downloading [====>] 1.2GB/4GB`。
///
/// 界面侧需要这些行来汇总成一条可读的「镜像层 / 估算进度」，所以不能在进入
/// `RunLog` 前整体丢掉；但把每一次重绘都写进持久日志，会在长下载尚未结束时把
/// 4 MiB 的诊断配额耗尽，真正的错误和结束结果反而落不进去。这里严格识别 Docker
/// 的层格式，只让它跳过**磁盘副本**。普通 Docker 信息、错误，以及任意非 Docker
/// 的 `hex: 内容` 行仍照常保留。
fn is_docker_layer_progress_line(line: &str) -> bool {
    let trimmed = line.trim();
    let body = trimmed
        .strip_prefix('[')
        .and_then(|rest| rest.split_once("] ").map(|(_, body)| body.trim()))
        .unwrap_or(trimmed);
    let Some((layer, status)) = body.split_once(": ") else {
        return false;
    };
    if !(8..=64).contains(&layer.len()) || !layer.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return false;
    }
    let status = status.trim().to_ascii_lowercase();
    [
        "pulling fs layer",
        "waiting",
        "downloading",
        "download complete",
        "extracting",
        "pull complete",
        "verifying checksum",
        "already exists",
        "layer already exists",
    ]
    .iter()
    .any(|prefix| status.starts_with(prefix))
}

// ---------------------------------------------------------------- 运行记录

/// 一次长操作的输出与结局。
///
/// 行存在内存里而不是只往外推：调用方可能晚到（界面切走再切回来、应用重启），
/// 没有回放就会丢掉前半段日志，而前半段往往正是出错的地方。
///
/// 内存那份有上限（`LOG_TAIL_LINES`/`LOG_TAIL_BYTES`）且保留期后整条消失；**完整的一份落在磁盘
/// 上**（`disk`）。两份的分工：内存那份服务于"现在正在看的界面"，磁盘那份服务于
/// "明天再来问为什么失败的人"。
struct RunLog {
    /// 这张任务卡属于哪张卡片、是哪种操作。本地下载服务（local_service）的
    /// /v1/tasks 要按这两个字段向 cmsd 透出任务表——登记表（ActiveCardOps）
    /// 只覆盖**还在跑**的运行，跑完的只能从这里拿。
    journal: Option<std::path::PathBuf>,
    persist_gate: Mutex<()>,
    last_checkpoint: Mutex<Instant>,
    card_id: String,
    kind: &'static str,
    install_context: Mutex<Option<super::incomplete::Context>>,
    retry_reference: Mutex<Option<super::incomplete::RetryReference>>,
    /// 创建时刻（RFC3339）。任务表透给cmsd排障用：「最近哪次下载失败了」
    /// 得能按时间排序回答。
    created_at: String,
    /// 单调递增序号：任务表排序用。created_at 是秒级往上的精度，同一秒内的
    /// 两个任务靠它分出先后。
    seq: u64,
    lines: Mutex<Vec<String>>,
    line_offset: Mutex<usize>,
    line_bytes: Mutex<usize>,
    last_progress: Mutex<Option<(u64, u64)>>,
    /// 落盘句柄（`run_log_store`，`~/.pinvou3/model-store/logs/`）。
    /// **开不出来就是 `None`，操作照常跑**——日志是诊断手段，不是功能，
    /// 为了记日志而让用户下不了模型是本末倒置。
    disk: Option<RunLogFile>,
    /// Docker 层的逐帧刷新不落盘时，留一条说明，避免诊断人员以为日志突然缺了一段。
    docker_progress_notice_emitted: Mutex<bool>,
    /// `None` = 还在跑；`Some(Ok(v))` = 成功及其返回值；`Some(Err(msg))` = 失败
    outcome: Mutex<Option<Result<Value, String>>>,
    /// 用户主动取消。取消在底层也是一个 Err，但对用户是两回事——"你点了取消"
    /// 和"它炸了"不该长得一样，调用方要能分别给出提示。
    cancelled: Mutex<bool>,
    /// 用户已放弃这条失败的安装：未完成面板不再列它，重试入口一并关死。
    /// 只作用于这一条回执（见 [`RunLog::dismiss`]）。
    dismissed: Mutex<bool>,
    /// 现在是不是有一条脚本提示在等人作答。
    ///
    /// 界面把「要不要弹输入框」从日志行里解析出来，而日志是可回放的——切走再
    /// 回来会从头读一遍。答过的提示被回放时必须**不能**再弹：用户填的答案会写进
    /// 一个没人来取的应答槽，下一次真正的提问一出现就被这份陈答案直接顶掉，
    /// 脚本于是拿着错误的输入继续往下跑（双机部署里那就是拿旧 IP 去连从机）。
    /// 所以"还在不在等"得由后端说了算，它是唯一知道真相的一方。
    awaiting_prompt: Mutex<bool>,
    /// 脚本有没有声明「失败后保留原生引擎进程」（`LMM_RETAINED_NATIVE_PROCESS=1`）。
    ///
    /// 声明过就**置位到底**：界面的「二次确认停用」入口靠它，而它以前只能从日志行
    /// 里认——回放一裁剪（前端只回放末尾 200 行）标志就丢，任务过期后残存进程占着
    /// 内存却没有任何常驻停用入口。和 `awaiting_prompt` 一样由后端结构化下发。
    retained_native: Mutex<bool>,
    finished_at_epoch: Mutex<Option<i64>>,
    finished_at: Mutex<Option<Instant>>,
    /// 有新行或结束时唤醒正在长轮询的调用
    notify: Arc<tokio::sync::Notify>,
}

/// 任务序号的进程内发号器。只增不减，任务表按它倒序就是「最新的在前」。
static RUN_SEQ: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

impl RunLog {
    fn new(card_id: &str, kind: &'static str, run_id: &str, active_run_ids: &[String]) -> Self {
        Self {
            journal: None,
            persist_gate: Mutex::new(()),
            last_checkpoint: Mutex::new(Instant::now()),
            card_id: card_id.to_string(),
            kind,
            install_context: Mutex::new(None),
            retry_reference: Mutex::new(None),
            created_at: chrono::Utc::now().to_rfc3339(),
            seq: RUN_SEQ.fetch_add(1, std::sync::atomic::Ordering::Relaxed),
            lines: Mutex::new(Vec::new()),
            line_offset: Mutex::new(0),
            line_bytes: Mutex::new(0),
            last_progress: Mutex::new(None),
            disk: RunLogFile::open(run_id, kind, active_run_ids),
            docker_progress_notice_emitted: Mutex::new(false),
            outcome: Mutex::new(None),
            cancelled: Mutex::new(false),
            dismissed: Mutex::new(false),
            awaiting_prompt: Mutex::new(false),
            retained_native: Mutex::new(false),
            finished_at_epoch: Mutex::new(None),
            finished_at: Mutex::new(None),
            notify: Arc::new(tokio::sync::Notify::new()),
        }
    }

    fn push(&self, line: &str) {
        // 提示行之后脚本就阻塞在 read 上，再出现任何一行输出就说明它已经拿到
        // 答案往下走了——这是日志流里唯一能看出"还在不在等"的地方。
        *self.awaiting_prompt.lock() = is_prompt_line(line);
        // 「失败后保留进程」是一次性声明，置位后不回退（后续行不会撤销它）。
        if line.trim() == "LMM_RETAINED_NATIVE_PROCESS=1" {
            *self.retained_native.lock() = true;
        }
        {
            let mut lines = self.lines.lock();
            let mut bytes = self.line_bytes.lock();
            let line = if line.len() > LOG_TAIL_BYTES {
                let mut end = LOG_TAIL_BYTES - 64;
                while !line.is_char_boundary(end) {
                    end -= 1;
                }
                format!("{} [log line truncated]", &line[..end])
            } else {
                line.to_owned()
            };
            if let Some(progress) = parse_progress(std::slice::from_ref(&line)) {
                *self.last_progress.lock() = Some(progress);
            }
            *bytes = bytes.saturating_add(line.len());
            lines.push(line);
            let mut remove = 0;
            while lines.len() - remove > LOG_TAIL_LINES || *bytes > LOG_TAIL_BYTES {
                *bytes = bytes.saturating_sub(lines[remove].len());
                remove += 1;
            }
            if remove > 0 {
                lines.drain(..remove);
                *self.line_offset.lock() += remove;
            }
        }
        // 落盘挂在这里、而不是在 pty 读取循环里另开一个出口：这一条是「已经
        // strip_ansi 过、已经切好行、已经加过阶段前缀」的唯一汇聚点，挂在它后面
        // 就自动继承了那些处理。落盘前还要过一道定向脱敏，见 run_log_store。
        // Docker 的层刷新例外：界面会聚合它，磁盘只留一次说明，以保证错误/结局
        // 不会被数万次重绘挤掉。
        if let Some(disk) = self.disk.as_ref() {
            if is_docker_layer_progress_line(line) {
                let mut emitted = self.docker_progress_notice_emitted.lock();
                if !*emitted {
                    disk.append("Docker 镜像层进度已在界面中汇总显示；日志省略重复的逐层刷新。");
                    *emitted = true;
                }
            } else {
                disk.append(line);
            }
        }
        let checkpoint = {
            let mut last = self.last_checkpoint.lock();
            if last.elapsed() >= Duration::from_secs(5) {
                *last = Instant::now();
                true
            } else {
                false
            }
        };
        if checkpoint {
            self.persist();
        }
        self.notify.notify_waiters();
    }

    /// 只在内存里的 RunLog，给测试用。
    ///
    /// 不让测试走 `new()`：那会在开发机/CI 的**真实** `~/.pinvou3/model-store/logs/`
    /// 下留文件。落盘本身有专门的文件级测试（`run_log_store`），用 `TempHome` 隔离。
    /// 不带 run_log_store 磁盘日志的 RunLog：冷回执恢复（journal 持久化照旧）
    /// 与测试共用。活着的运行走 [`RunLog::new`]，由它开磁盘日志并参与轮转。
    fn new_without_disk(card_id: &str, kind: &'static str) -> Self {
        Self {
            journal: None,
            persist_gate: Mutex::new(()),
            last_checkpoint: Mutex::new(Instant::now()),
            card_id: card_id.to_string(),
            kind,
            install_context: Mutex::new(None),
            retry_reference: Mutex::new(None),
            created_at: chrono::Utc::now().to_rfc3339(),
            seq: RUN_SEQ.fetch_add(1, std::sync::atomic::Ordering::Relaxed),
            lines: Mutex::new(Vec::new()),
            line_offset: Mutex::new(0),
            line_bytes: Mutex::new(0),
            last_progress: Mutex::new(None),
            disk: None,
            docker_progress_notice_emitted: Mutex::new(false),
            outcome: Mutex::new(None),
            cancelled: Mutex::new(false),
            awaiting_prompt: Mutex::new(false),
            retained_native: Mutex::new(false),
            finished_at_epoch: Mutex::new(None),
            finished_at: Mutex::new(None),
            notify: Arc::new(tokio::sync::Notify::new()),
        }
    }

    #[cfg(test)]
    fn new_memory_only() -> Self {
        Self {
            journal: None,
            persist_gate: Mutex::new(()),
            last_checkpoint: Mutex::new(Instant::now()),
            card_id: String::new(),
            kind: "download",
            install_context: Mutex::new(None),
            retry_reference: Mutex::new(None),
            created_at: chrono::Utc::now().to_rfc3339(),
            seq: RUN_SEQ.fetch_add(1, std::sync::atomic::Ordering::Relaxed),
            lines: Mutex::new(Vec::new()),
            line_offset: Mutex::new(0),
            line_bytes: Mutex::new(0),
            last_progress: Mutex::new(None),
            disk: None,
            docker_progress_notice_emitted: Mutex::new(false),
            outcome: Mutex::new(None),
            cancelled: Mutex::new(false),
            awaiting_prompt: Mutex::new(false),
            retained_native: Mutex::new(false),
            finished_at_epoch: Mutex::new(None),
            finished_at: Mutex::new(None),
            notify: Arc::new(tokio::sync::Notify::new()),
        }
    }

    /// 界面答完了。不等脚本打出下一行就落下标志：用户完全可能答完立刻切走，
    /// 那一瞬间再回来的话，回放会把同一条提示又弹一次。
    fn answered(&self) {
        *self.awaiting_prompt.lock() = false;
    }

    /// 用户放弃了这条失败的安装。只打标并落盘，不删任何文件。
    ///
    /// 调用方（dismiss_incomplete_local）已经按磁盘上的回执做过完整校验；这里
    /// 再拦一次"没终结"，是因为内存里的 log 和磁盘文件由同一路 persist 保持
    /// 一致，但校验和改写之间仍可能插入一次 finish——兜住这条窄竞态的代价是
    /// 一个 if，漏过去的后果是活任务的记录被打上"已放弃"。
    fn dismiss(&self) -> Result<(), String> {
        if self.outcome.lock().is_none() {
            return Err("installation_not_terminal".into());
        }
        *self.dismissed.lock() = true;
        self.persist_result()
            .map_err(|error| format!("installation_dismiss_persist_failed: {error}"))
    }

    fn finish(&self, outcome: Result<Value, String>, cancelled: bool) {
        if let Some(disk) = self.disk.as_ref() {
            let text = if cancelled {
                "已取消".to_string()
            } else {
                match &outcome {
                    Ok(_) => "成功".to_string(),
                    Err(e) => format!("失败：{e}"),
                }
            };
            disk.finish(&text);
        }
        *self.cancelled.lock() = cancelled;
        *self.awaiting_prompt.lock() = false;
        *self.outcome.lock() = Some(outcome);
        *self.finished_at_epoch.lock() = Some(chrono::Utc::now().timestamp());
        *self.finished_at.lock() = Some(Instant::now());
        self.persist();
        self.notify.notify_waiters();
    }

    fn snapshot(
        &self,
        from: usize,
        options: super::log_page::Options,
    ) -> Result<(super::log_page::Page, Option<Result<Value, String>>), String> {
        // Read terminal state first. Once it is present no producer can append
        // a final line after this snapshot, so terminal paging needs no catch-up.
        let outcome = self.outcome.lock().clone();
        let lines = self.lines.lock();
        let offset = *self.line_offset.lock();
        let mut options = options;
        if from < offset {
            options.from_byte = 0;
        }
        let mut page = super::log_page::page(&lines, from.saturating_sub(offset), options)?;
        page.from += offset;
        page.next += offset;
        page.total += offset;
        for line in &mut page.truncated_lines {
            *line += offset;
        }
        if let Some(fragment) = &mut page.fragment {
            fragment.line += offset;
        }
        Ok((page, outcome))
    }
}

// ---------------------------------------------------------------- 服务状态

/// 模型商店的进程内状态。作为 Tauri 的 managed state 注册一次。
///
/// `active` / `queue` 必须是**全进程唯一**的：它们是卡片级互斥和下载并发的唯一
/// 依据，各持一份等于没有约束。真机上踩过——两个 `hf download` 抢同一批文件的锁，
/// 速度从 2.2MB/s 掉到 569KB/s。
pub struct ModelStoreService {
    remote: bool,
    pub(super) host_control: super::host_control::HostControl,
    accept_gate: Mutex<()>,
    pub(super) lan: super::lan_gateway::Gateway,
    registry: CancelRegistry,
    active: Arc<ActiveCardOps>,
    queue: Arc<DownloadQueue>,
    runs: Mutex<HashMap<String, Arc<RunLog>>>,
    startup_cleanup: StartupCleanup,
    startup_updates: super::startup_updates::StartupUpdates,
    models_changed: Mutex<Option<super::super::super::ModelsChangedHandler>>,
    linked_revision: Mutex<Option<String>>,
    linked_runtime: Mutex<super::linked_models::RuntimeFacts>,
}

impl Default for ModelStoreService {
    fn default() -> Self {
        Self::new()
    }
}

impl ModelStoreService {
    pub fn for_desktop() -> Self {
        Self::with_desktop_host(super::background::ensure_host)
    }

    fn with_desktop_host(ensure_host: impl FnOnce() -> Result<(), String>) -> Self {
        let mut service = Self::new();
        // The Linux store owns durable work before cmsd is installed as well.
        // A fixed host binding avoids a second queue when setup completes while
        // this desktop already has local work in flight. Startup failure never
        // authorizes an independent in-process executor.
        service.remote = true;
        if let Err(e) = ensure_host() {
            eprintln!("[model-store] {e}");
        }
        service
    }

    pub(super) fn retire_host(
        &self,
        expected: &super::host_control::Identity,
    ) -> Result<(), String> {
        self.host_control.retire(expected, || {
            let _accept = self.accept_gate.lock();
            let runs = self.runs.lock();
            if runs.values().any(|log| log.outcome.lock().is_none()) {
                return Err("store_host_busy: accepted tasks are still active".into());
            }
            super::orphans::ensure_host_quiet()?;
            for log in runs.values() {
                log.persist_result()
                    .map_err(|e| format!("task_persist_failed: {e}"))?;
            }
            Ok(())
        })
    }

    pub fn is_remote(&self) -> bool {
        self.remote
    }

    pub(super) fn restore_runs(&self) -> Result<(), String> {
        let root = config::data_dir().join("runs");
        if !root.exists() {
            return Ok(());
        }
        for entry in std::fs::read_dir(root).map_err(|e| e.to_string())? {
            let path = entry.map_err(|e| e.to_string())?.path();
            if path.extension().and_then(|s| s.to_str()) != Some("json") {
                continue;
            }
            let log = match load_saved_run(&path) {
                Ok(Some(log)) => log,
                Ok(None) => continue,
                Err(error) => {
                    // Keep the unreadable receipt: exact-ID retries still fail
                    // closed, but unrelated tasks must remain available.
                    log::warn!(
                        "model-store task restore skipped {}: {error}",
                        path.display()
                    );
                    continue;
                }
            };
            // Cold receipts remain on disk for exact-ID replay, cancelled/failed
            // installation proofs and maintenance recovery. Reopening the host
            // must not make old terminal logs hot for another seven days.
            if !log.is_hot() {
                log.expire_log_tail();
            }
            if log.is_hot() {
                if let Some(id) = path.file_stem().and_then(|s| s.to_str()) {
                    self.runs.lock().insert(id.to_string(), Arc::new(log));
                }
            }
        }
        Ok(())
    }

    pub fn new() -> Self {
        Self {
            remote: false,
            host_control: super::host_control::HostControl::new(),
            accept_gate: Mutex::new(()),
            lan: super::lan_gateway::Gateway::default(),
            registry: CancelRegistry::default(),
            active: Arc::new(ActiveCardOps::default()),
            queue: Arc::new(DownloadQueue::default()),
            runs: Mutex::new(HashMap::new()),
            startup_cleanup: StartupCleanup::default(),
            startup_updates: Default::default(),
            models_changed: Mutex::new(None),
            linked_revision: Mutex::new(None),
            linked_runtime: Mutex::new(Default::default()),
        }
    }

    pub fn set_models_changed_handler(&self, handler: super::super::super::ModelsChangedHandler) {
        *self.models_changed.lock() = Some(handler);
    }

    fn observe_linked_runtime(&self, payload: &Value) -> Result<Vec<String>, String> {
        let pending = if payload["link_sync_error"].is_null() {
            self.linked_runtime.lock().observe(payload)
        } else {
            super::linked_models::RuntimeFacts::pending_cards(payload)
        };
        self.notify_linked_snapshot()?;
        Ok(pending)
    }

    fn notify_linked_snapshot(&self) -> Result<(), String> {
        let (saved_revision, ids) = super::linked_models::snapshot()?;
        let runtime_revision = self.linked_runtime.lock().revision(&ids);
        let revision = super::script_runner::sha256_hex(
            &json!([saved_revision, runtime_revision]).to_string(),
        );
        let changed = self
            .linked_revision
            .lock()
            .replace(revision.clone())
            .as_ref()
            != Some(&revision);
        if changed && !ids.is_empty() {
            if let Some(handler) = self.models_changed.lock().clone() {
                handler(&ids);
            }
        }
        Ok(())
    }

    fn notify_models_changed(&self, changed: &[String]) {
        if !changed.is_empty() {
            if let Some(handler) = self.models_changed.lock().clone() {
                handler(changed);
            }
        }
    }

    fn refresh_enabled_model(&self, card_id: &str) -> Result<(), String> {
        super::linked_models::sync(Some(card_id))?;
        super::linked_models::mark_runtime_refresh(card_id)?;
        self.notify_linked_snapshot()
    }

    fn sync_linked_models(&self, card_id: Option<&str>) -> Result<(), String> {
        super::linked_models::sync(card_id)?;
        self.notify_linked_snapshot()
    }

    fn new_run(
        &self,
        run_id: &str,
        card_id: &str,
        kind: &'static str,
    ) -> Result<Arc<RunLog>, String> {
        self.new_run_with_metadata(run_id, card_id, kind, None, None)
    }

    fn new_run_with_metadata(
        &self,
        run_id: &str,
        card_id: &str,
        kind: &'static str,
        context: Option<super::incomplete::Context>,
        retry: Option<super::incomplete::RetryReference>,
    ) -> Result<Arc<RunLog>, String> {
        self.sweep_old_runs();
        // 新建日志前把「现有活跃 + 自己」交给轮转：正在跑的运行日志不能被
        // prune 删掉（等输入的静默期里 mtime 会变旧，恰好落在删除线上）。
        let mut active: Vec<String> = self.runs.lock().keys().cloned().collect();
        active.push(run_id.to_string());
        let mut log = RunLog::new(card_id, kind, run_id, &active);
        *log.install_context.lock() = context;
        *log.retry_reference.lock() = retry;
        log.journal = Some(
            config::data_dir()
                .join("runs")
                .join(format!("{run_id}.json")),
        );
        let log = Arc::new(log);
        log.persist_result()
            .map_err(|e| format!("task_persist_failed: {e}"))?;
        self.runs
            .lock()
            .insert(run_id.to_string(), Arc::clone(&log));
        Ok(log)
    }

    fn lookup_run(&self, run_id: &str) -> Result<Option<Arc<RunLog>>, String> {
        if let Some(log) = self.runs.lock().get(run_id).cloned() {
            return Ok(Some(log));
        }
        let safe = config::safe_path_component(run_id)?;
        let path = config::data_dir().join("runs").join(format!("{safe}.json"));
        match std::fs::symlink_metadata(&path) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(format!("task_history_unavailable: {error}")),
            Ok(_) => load_saved_run(&path)?
                .map(|log| Some(Arc::new(log)))
                .ok_or_else(|| "task_history_unknown_kind: original receipt retained".into()),
        }
    }

    fn run(&self, run_id: &str) -> Option<Arc<RunLog>> {
        match self.lookup_run(run_id) {
            Ok(log) => log,
            Err(error) => {
                log::warn!("model-store task lookup failed: {error}");
                None
            }
        }
    }

    /// Operator escape for a lost receiver record. The accept gate serializes
    /// this tombstone with late requests using the same id. No running operation
    /// or orphan writer may exist; corrupt receipts never count as absence.
    pub(super) fn abandon_missing_run(
        &self,
        run_id: &str,
        card_id: &str,
        kind: &str,
        original_created_at: u64,
    ) -> Result<(), String> {
        config::safe_path_component(run_id)?;
        config::safe_path_component(card_id)?;
        let kind = match kind {
            "download" => "download",
            "install_local" => "install_local",
            _ => return Err("invalid_installation_kind".into()),
        };
        let _accept = self.accept_gate.lock();
        if self.lookup_run(run_id)?.is_some() {
            return Err("task_already_recorded: 请核对原任务结果".into());
        }
        if self
            .runs
            .lock()
            .values()
            .any(|log| log.outcome.lock().is_none())
        {
            return Err("store_host_busy: 商店仍有进行中的任务".into());
        }
        super::orphans::ensure_host_quiet()?;
        // Loss of the task database may also erase the orphan registry. A
        // missing registry cannot prove that a previous script has exited.
        // Require a device boot after this immutable cmsd task admission;
        // inspect only kernel evidence, never a caller-supplied boot timestamp.
        let boot = std::fs::read_to_string("/proc/stat")
            .ok()
            .and_then(|text| {
                text.lines().find_map(|line| {
                    line.strip_prefix("btime ")
                        .and_then(|value| value.trim().parse::<u64>().ok())
                })
            })
            .filter(|boot| *boot > 0)
            .ok_or("installation_boot_evidence_unavailable")?;
        if original_created_at == 0 || original_created_at >= boot {
            // r23 R5：原文案内嵌中文句子，en/ja 界面渲染中文原文；
            // 改纯 code，文案由 cmsd-service/incompleteErrors 三语表承接。
            return Err("lost_task_reboot_required".into());
        }
        let log = self.new_run(run_id, card_id, kind)?;
        log.finish(
            Err(
                "operator_abandoned: 原任务记录丢失，已确认无存活脚本；保留文件，未重跑安装".into(),
            ),
            true,
        );
        log.persist_result()
            .map_err(|error| format!("task_persist_failed: {error}"))
    }

    fn sweep_old_runs(&self) {
        self.runs.lock().retain(|_, log| {
            if log.is_hot() {
                true
            } else {
                log.expire_log_tail();
                false
            }
        });
    }
}

/// 启动时的 linked-models 后台对账：不等用户打开商店视图就把路由/上下文
/// 修复对账做完，失败只记日志（打开视图时会重试）。
///
/// cmsd 请求宿主退出（retire）时必须等这笔启动偏好事务做完，所以整个事务
/// 持有 `host_control` 的请求守卫；宿主已在排水就直接放弃这次对账。
pub fn run_startup_linked_sync(service: &Arc<ModelStoreService>) {
    let service = Arc::clone(service);
    std::thread::spawn(move || {
        let Ok(_request) = service.host_control.request() else {
            return;
        };
        if let Err(error) = service.sync_linked_models(None) {
            log::warn!("model-store linked model refresh failed: {error}");
        }
    });
}

#[derive(serde::Serialize, serde::Deserialize)]
struct SavedRun {
    card_id: String,
    kind: String,
    created_at: String,
    lines: Vec<String>,
    #[serde(default)]
    line_offset: usize,
    #[serde(default)]
    progress: Option<(u64, u64)>,
    outcome: Option<Result<Value, String>>,
    cancelled: bool,
    #[serde(default)]
    finished_at_epoch: Option<i64>,
    #[serde(default)]
    install_context: Option<super::incomplete::Context>,
    #[serde(default)]
    retry_reference: Option<super::incomplete::RetryReference>,
    /// 用户已放弃这条失败的安装：不再提醒，也不再提供重试。只打标，不删文件——
    /// 回执是耐久任务事实，删除会把同卡更早的失败记录顶上来，弹窗换个 ID 复活。
    #[serde(default)]
    dismissed: bool,
}

/// Restore old terminal receipts without changing their completion age. Legacy
/// journals predate the explicit epoch; their existing file mtime is the last
/// known completion/checkpoint, and is preserved instead of using startup time.
fn load_saved_run(path: &std::path::Path) -> Result<Option<RunLog>, String> {
    use std::os::unix::fs::OpenOptionsExt;
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|e| e.to_string())?;
    let metadata = file.metadata().map_err(|e| e.to_string())?;
    if !metadata.is_file() {
        return Err("task_history_untrusted".into());
    }
    // Legacy journals can exceed current producer limits. Decode them from
    // the checked descriptor; cold cleanup removes only expired log content.
    let saved: SavedRun = serde_json::from_reader(std::io::BufReader::new(file))
        .map_err(|e| format!("任务记录损坏 {}: {e}", path.display()))?;
    let kind = match saved.kind.as_str() {
        "download" => "download",
        "install_local" => "install_local",
        "deploy" => "deploy",
        "enable" => "enable",
        "stop" => "stop",
        "uninstall" => "uninstall",
        "uninstall_recovery" => "uninstall_recovery",
        "update" => "update",
        "version_stage" => "version_stage",
        "version_switch" => "version_switch",
        "version_delete" => "version_delete",
        _ => return Ok(None),
    };
    // 恢复的是冷回执：不重开 run_log_store 磁盘日志（那份属于"活着的运行"，
    // 由 new() 在任务启动时创建），journal 持久化照旧。
    let mut log = RunLog::new_without_disk(&saved.card_id, kind);
    *log.install_context.lock() = saved.install_context;
    *log.retry_reference.lock() = saved.retry_reference;
    log.created_at = saved.created_at;
    log.journal = Some(path.to_path_buf());
    *log.last_progress.lock() = saved.progress.or_else(|| parse_progress(&saved.lines));
    // Apply current limits to legacy checkpoints too. Preserve the original
    // absolute cursor and progress even when their source lines are retired.
    let mut lines = saved.lines;
    let mut changed = false;
    for line in &mut lines {
        if line.len() > LOG_TAIL_BYTES {
            let mut end = LOG_TAIL_BYTES - 64;
            while !line.is_char_boundary(end) {
                end -= 1;
            }
            line.truncate(end);
            line.push_str(" [log line truncated]");
            changed = true;
        }
    }
    let mut bytes: usize = lines.iter().map(String::len).sum();
    let mut removed = 0;
    while lines.len() - removed > LOG_TAIL_LINES || bytes > LOG_TAIL_BYTES {
        bytes -= lines[removed].len();
        removed += 1;
    }
    changed |= removed > 0;
    lines.drain(..removed);
    *log.line_offset.lock() = saved.line_offset.saturating_add(removed);
    *log.line_bytes.lock() = bytes;
    *log.lines.lock() = lines;
    *log.cancelled.lock() = saved.cancelled;
    *log.dismissed.lock() = saved.dismissed;
    if let Some(outcome) = saved.outcome {
        *log.outcome.lock() = Some(outcome);
        *log.finished_at_epoch.lock() = saved.finished_at_epoch.or_else(|| {
            metadata
                .modified()
                .ok()?
                .duration_since(std::time::UNIX_EPOCH)
                .ok()
                .and_then(|duration| i64::try_from(duration.as_secs()).ok())
        });
    } else {
        log.finish(Err(if matches!(kind, "uninstall" | "uninstall_recovery") {
            "interrupted: 商店后台服务曾中断，未自动重跑卸载。请在模型商店核实并恢复维护；文件和部署状态需要检查。"
        } else {
            "interrupted: 商店后台服务曾中断，原任务不会自动重发。已保留下载成果；请核实设备状态后重试安装以继续断点。"
        }.into()), saved.cancelled);
    }
    if changed {
        log.persist_result().map_err(|error| error.to_string())?;
    }
    Ok(Some(log))
}

impl RunLog {
    fn expire_log_tail(&self) {
        {
            let mut lines = self.lines.lock();
            if lines.is_empty() {
                return;
            }
            *self.line_offset.lock() += lines.len();
            lines.clear();
            *self.line_bytes.lock() = 0;
        }
        // Retain identity, outcome, installation context and completion age:
        // cold history still fences exact-ID replays and supports recovery.
        self.persist();
    }

    fn is_hot(&self) -> bool {
        self.finished_at_epoch.lock().is_none_or(|epoch| {
            chrono::Utc::now().timestamp().saturating_sub(epoch) < RUN_RETENTION.as_secs() as i64
        })
    }

    fn set_install_context(&self, context: super::incomplete::Context) -> Result<(), String> {
        *self.install_context.lock() = Some(context);
        self.persist_result()
            .map_err(|error| format!("installation_context_persist_failed: {error}"))
    }

    fn persist(&self) {
        if let Err(e) = self.persist_result() {
            eprintln!("[model-store] persist task failed: {e}");
        }
    }

    fn persist_result(&self) -> std::io::Result<()> {
        let Some(path) = self.journal.as_ref() else {
            return Ok(());
        };
        let _gate = self.persist_gate.lock();
        // Keep the tail and its absolute origin in one observation. A producer
        // may retire lines while a checkpoint is being serialized.
        let (lines, line_offset, progress) = {
            let lines = self.lines.lock();
            (
                lines.clone(),
                *self.line_offset.lock(),
                *self.last_progress.lock(),
            )
        };
        let saved = SavedRun {
            card_id: self.card_id.clone(),
            kind: self.kind.to_string(),
            created_at: self.created_at.clone(),
            lines,
            line_offset,
            progress,
            outcome: self.outcome.lock().clone(),
            cancelled: *self.cancelled.lock(),
            dismissed: *self.dismissed.lock(),
            finished_at_epoch: *self.finished_at_epoch.lock(),
            install_context: self.install_context.lock().clone(),
            retry_reference: self.retry_reference.lock().clone(),
        };
        let bytes = serde_json::to_vec(&saved)?;
        super::execution_fence::private_write(path, &bytes).map_err(std::io::Error::other)
    }
}

// ---------------------------------------------------------------- 辅助

/// 生成一个新的 run_id。
///
/// 由服务端发号而不是让前端带：前端可能因为重渲染或用户连点而发两次同样的调用，
/// 各自拿到独立的 id 才不会互相覆盖日志。
fn new_run_id() -> String {
    use base64::Engine;
    use rand::Rng as _;
    let mut buf = [0u8; 12];
    rand::rng().fill_bytes(&mut buf);
    format!(
        "run-{}",
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(buf)
    )
}

/// 把 panic 的 payload 抠成一句能给人看的话。`panic!("……")` 的 payload 是
/// `String`，`panic!("字面量")` 是 `&str`，其余（比如 `panic_any`）只能认栽。
fn panic_text(payload: &(dyn std::any::Any + Send)) -> String {
    if let Some(s) = payload.downcast_ref::<String>() {
        s.clone()
    } else if let Some(s) = payload.downcast_ref::<&str>() {
        s.to_string()
    } else {
        "未知原因".to_string()
    }
}

/// 这个模型需不需要重新部署：部署用的 deploy 脚本跟现在手上的对不上就要。
///
/// 它是一条**派生判定**，不在 `StoreModelRecord`（那是库表的投影）里。界面靠它
/// 决定「重新部署」按钮出不出现——没有它就只能对所有已部署模型常驻这个按钮，
/// 用户根本不知道自己为什么要点。
/// 轻量版判定（P2-4）：deploy 脚本哈希从摘要映射取，判据与全量读正文的旧版
/// 相同（含老记录的时间戳回退），并集主线 card_update_pending 整包更新门。
fn needs_redeploy_light(
    record: &db::StoreModelRecord,
    hashes: &std::collections::BTreeMap<String, String>,
) -> bool {
    if record.status != "deployed" {
        return false;
    }
    // 主线并集：整包更新在等重新部署时也算「需要重新部署」。
    if record.card_update_pending {
        return true;
    }
    if let (Some(deployed), Some(current)) = (
        record.deployed_script_sha256.as_deref(),
        hashes.get("deploy").map(|s| s.as_str()),
    ) {
        return deployed != current;
    }
    match (
        record.scripts_fetched_at.as_deref(),
        record.deployed_at.as_deref(),
    ) {
        (Some(fetched), Some(deployed_at)) => fetched > deployed_at,
        _ => false,
    }
}

/// 列表接口用：把每条记录里"脚本正文"那两个大字段摘掉。
///
/// `scripts_content` / `assets_content` 装的是每张卡**全部脚本的正文**，实测五张卡
/// 就把这个响应撑到 8.6 MB。界面每做完一个操作都会刷一次列表，等于每次都从 IPC
/// 搬 8.6 MB 再解析一遍，肉眼可见地卡。而列表页一个字都用不到它们——要看脚本正文
/// 请求详情接口，那里原样保留。
///
/// 摘不动（序列化失败、不是数组/对象）时原样返回：这只是个瘦身，不该因为它让
/// 整个接口失败。
fn strip_bulky_fields<T: serde::Serialize>(models: T) -> Value {
    const BULKY: [&str; 2] = ["scripts_content", "assets_content"];
    let Ok(mut value) = serde_json::to_value(models) else {
        return Value::Null;
    };
    if let Some(items) = value.as_array_mut() {
        for item in items.iter_mut() {
            if let Some(obj) = item.as_object_mut() {
                for key in BULKY {
                    obj.remove(key);
                }
            }
        }
    }
    value
}

/// 列表记录：瘦身之后再补上 `needs_redeploy`。
///
/// 两件事的顺序不能反：判定要读 `scripts_content` 里 deploy 脚本的哈希，而
/// `strip_bulky_fields` 正是要把 `scripts_content` 摘掉。先摘后算的话这个字段会
/// 永远是 false，界面上「重新部署」就永远不出现——比常驻还糟，因为脚本真的更新了
/// 也没人提示。
fn library_payload(rows: Vec<db::LibraryRow>) -> Value {
    // r5 评审 P2-4：改走 `list_store_models_light`——正文一个字节不出库（此前
    // SELECT * 读入 ~8.6MB 再在应用层丢掉，且每次界面操作都触发）。派生判定
    // 从轻量行的 sha256 摘要算，语义与原先读正文时一致。
    let flags: Vec<bool> = rows
        .iter()
        .map(|row| needs_redeploy_light(&row.record, &row.script_hashes))
        .collect();
    // 本地脚本哈希摘要（script_type -> sha256）。前端拿它跟卡片列表的
    // script_sha256 比对，自动检测「服务器有更新」——用户不需要手动检查。
    let local_hashes: Vec<Value> = rows
        .iter()
        .map(|row| {
            let mut map = serde_json::Map::new();
            for (kind, sha) in &row.script_hashes {
                map.insert(kind.clone(), Value::String(sha.clone()));
            }
            Value::Object(map)
        })
        .collect();
    let defaults: Vec<_> = rows
        .iter()
        .map(|row| {
            super::deploy_profiles::default_id(&row.record.card_id, &row.record.profiles)
                .map(str::to_owned)
        })
        .collect();
    let revisions: Vec<_> = rows
        .iter()
        .map(|row| {
            // 轻量行的正文列是空占位——manifest 必须从逐文件摘要构建
            // （与 startup_card_manifest 同一套 digest 语义），否则算出的
            // revision 永远不等于启动检查的 local_revision，「有更新」徽标
            // 永远不亮。
            super::card_update::revision(&super::card_update::notice_manifest_from_digests(
                &row.record,
                &row.script_hashes,
                &row.asset_hashes,
            ))
        })
        .collect();
    let models: Vec<db::StoreModelRecord> = rows.into_iter().map(|row| row.record).collect();
    let mut value = strip_bulky_fields(models);
    if let Some(items) = value.as_array_mut() {
        for ((((item, needs), hashes), default), revision) in items
            .iter_mut()
            .zip(flags)
            .zip(local_hashes)
            .zip(defaults)
            .zip(revisions)
        {
            if let Some(obj) = item.as_object_mut() {
                obj.insert("needs_redeploy".to_string(), Value::Bool(needs));
                obj.insert("local_script_hashes".to_string(), hashes);
                obj.insert("default_profile_id".to_string(), json!(default));
                obj.insert("card_revision".to_string(), json!(revision));
            }
        }
    }
    value
}

// ---------------------------------------------------------------- 长操作骨架

/// 收尾守卫。
///
/// 四件收尾事情挂在 `Drop` 上，而不是顺着写在 job 后面。以前是后者，于是 job 一
/// panic 就一件都不做：卡片在 `ActiveCardOps` 里被**永久锁死**（此后一律拒绝）、
/// `RunLog::finish` 不被调用导致长轮询永远不返回、下载名额不释放、运行记录也清不掉。
struct OpCleanup {
    _card_guard: Option<super::runtime_guard::StartupGuard>,
    registry_owner: Arc<ModelStoreService>,
    card: String,
    run_id: String,
    log: Arc<RunLog>,
    control: script_runner::RunControl,
    /// 正常跑完时由工作线程填上；还是 `None` 就说明 panic 逃出了 catch_unwind，
    /// 那也得给 RunLog 一个结局，不能让调用方一直等下去。
    outcome: Option<Result<Value, String>>,
    operation_lock: Option<std::fs::File>,
}

impl Drop for OpCleanup {
    fn drop(&mut self) {
        self.registry_owner.registry.remove(&self.run_id);
        self.registry_owner.active.finish(&self.card);
        // 下载之外的操作没进过队列，release 对它们是空操作
        self.registry_owner.queue.release(&self.run_id);
        self.operation_lock.take();
        // 合并修正（r23）：main 侧新增的 card-<sha> StartupGuard 也要在同一时刻
        // 放开——它此前靠结构体字段收尾隐式释放，落在 finish 置位之后，长轮询
        // 的调用方会在 outcome 一出现就立刻重试同卡，撞上还没放开的操作保护
        // （card_operation_lock_is_shared_by_independent_service_instances 钉住此序）。
        self._card_guard.take();
        // finish 必须是最后一步：它一置位，长轮询就会立刻带着结局返回，调用方
        // 可能下一毫秒就来重试同一张卡——那时互斥必须已经放开了。
        let outcome = self
            .outcome
            .take()
            .unwrap_or_else(|| Err("内部错误：任务线程异常终止，没有留下结果".to_string()));
        let cancelled = self.control.cancelled() && outcome.is_err();
        self.log.finish(outcome, cancelled);
    }
}

/// Cross-process serialization for all mutations of one public card.
fn card_operation_lock(card_id: &str) -> Result<std::fs::File, String> {
    let safe_id = config::safe_path_component(card_id)?;
    let lock_dir = config::data_dir().join("operation-locks");
    std::fs::create_dir_all(&lock_dir).map_err(|e| e.to_string())?;
    // Keep the established path: moving a live lock would split exclusion
    // between processes running different app versions.
    let operation_lock = crate::platform::filesystem::open_private_lock_file(
        &lock_dir.join(format!("{safe_id}.lock")),
    )
    .map_err(|e| e.to_string())?;
    operation_lock
        .try_lock()
        .map_err(|_| "该模型正在其他窗口操作，请稍后重试".to_string())?;
    // No previous operation can still own this card. Recover its stale plans
    // before a new revision starts and the card lock becomes deliberately busy.
    super::version_store::recover_interrupted_while_locked(card_id, &operation_lock)?;
    Ok(operation_lock)
}

/// 把一个阻塞的生命周期操作跑起来，立刻返回 run_id。
///
/// 用 `std::thread` 而不是 tokio 的阻塞池：这些操作动辄几十分钟（下 168GB 权重、
/// 加载模型），占着阻塞池的名额会把别的任务饿死，而且它们内部是纯同步的 pty 循环，
/// 本来就不需要异步运行时。
fn spawn_op<F>(
    service: &Arc<ModelStoreService>,
    card_id: &str,
    op: CardOp,
    job: F,
) -> Result<Value, String>
where
    F: FnOnce(&Arc<RunLog>, script_runner::RunControl, &str) -> Result<Value, String>
        + Send
        + 'static,
{
    spawn_op_with_id(service, card_id, op, None, job)
}

fn spawn_op_with_id<F>(
    service: &Arc<ModelStoreService>,
    card_id: &str,
    op: CardOp,
    requested_run_id: Option<String>,
    job: F,
) -> Result<Value, String>
where
    F: FnOnce(&Arc<RunLog>, script_runner::RunControl, &str) -> Result<Value, String>
        + Send
        + 'static,
{
    spawn_op_with_metadata(service, card_id, op, requested_run_id, None, job)
}

fn spawn_op_with_metadata<F>(
    service: &Arc<ModelStoreService>,
    card_id: &str,
    op: CardOp,
    requested_run_id: Option<String>,
    retry: Option<(
        super::incomplete::Context,
        super::incomplete::RetryReference,
    )>,
    job: F,
) -> Result<Value, String>
where
    F: FnOnce(&Arc<RunLog>, script_runner::RunControl, &str) -> Result<Value, String>
        + Send
        + 'static,
{
    // run_id 要在占坑之前先发出来：互斥登记里存着它，界面切走再回来时正是按
    // 卡片把它要回去接着读日志的（见 `ActiveCardOps::run_for_card`）。
    // 但登记表之外的东西一律等占坑成功再建——占不到坑就直接返回，不能留下一条
    // 没人会去读、也没人会去收的运行记录。
    let _accept = service.accept_gate.lock();
    service.host_control.ensure_accepting()?;
    let run_id = requested_run_id.unwrap_or_else(new_run_id);
    if let Some(log) = service.lookup_run(&run_id)? {
        if log.card_id != card_id
            || log.kind != op.kind
            || log.retry_reference.lock().as_ref() != retry.as_ref().map(|(_, reference)| reference)
        {
            return Err("idempotency_conflict".into());
        }
        return Ok(json!({"run_id": run_id}));
    }
    if let Some((context, reference)) = retry.as_ref() {
        let (card, kind, current) = super::incomplete::retry_request(&reference.original_run_id)?;
        if card != card_id || kind != op.kind || &current != context {
            return Err("installation_preview_changed".into());
        }
    }
    if op != OP_UNINSTALL_RECOVERY {
        super::maintenance::ensure_clear(card_id)?;
    }
    // All lifecycle actions share the lock with package replacement, including
    // another app process. Closing the fd releases it after panic/cancellation.
    let operation_lock = card_operation_lock(card_id)?;
    // PR 侧的按卡保护锁（哈希文件名，任意 card_id 可用）：与上面的
    // operation-lock 互为补充，混跑新旧版本的应用窗口时两把都能对上。
    let card_guard = super::runtime_guard::StartupGuard::for_card(card_id)?;
    // 卡片级互斥：同一张卡不许被两处同时操作。
    service.active.try_begin(card_id, op, Some(&run_id))?;

    let created = match retry {
        Some((context, reference)) => {
            service.new_run_with_metadata(&run_id, card_id, op.kind, Some(context), Some(reference))
        }
        None => service.new_run(&run_id, card_id, op.kind),
    };
    let log = match created {
        Ok(log) => log,
        Err(error) => {
            service.active.finish(card_id);
            return Err(error);
        }
    };
    let control = service.registry.register(&run_id);

    let svc = Arc::clone(service);
    let card = card_id.to_string();
    let rid = run_id.clone();
    let log_for_thread = Arc::clone(&log);
    // 留一份给结束后回查：control 本身要 move 进 job
    let control_probe = control.clone();
    let rid_for_job = run_id.clone();
    // Construct before spawning: if the OS cannot allocate a worker, dropping
    // the closure must still release the card slot and finish the run log.
    let guard = OpCleanup {
        _card_guard: Some(card_guard),
        registry_owner: svc,
        card,
        run_id: rid,
        log: Arc::clone(&log_for_thread),
        control: control_probe,
        outcome: Some(Err("无法创建模型操作线程，请稍后重试。".into())),
        operation_lock: Some(operation_lock),
    };
    std::thread::Builder::new()
        .name("model-store-job".into())
        .spawn(move || {
            // Move the complete Drop guard, not just the outcome field through
            // disjoint closure capture. Cleanup belongs to this worker's scope.
            let mut guard = guard;
            // catch_unwind 只负责把 panic 变成一句人话，"一定会收尾"是守卫保证的。
            // AssertUnwindSafe：job 只被调用一次，panic 之后我们不再碰它捕获的任何
            // 状态，唯一跨过边界的 RunLog / ActiveCardOps 都在锁后面（parking_lot
            // 的锁不会因为持锁线程 panic 而中毒，拿到的是已写完的一致状态）。
            let caught = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                job(&log_for_thread, control, &rid_for_job)
            }));
            guard.outcome = Some(match caught {
                Ok(outcome) => outcome,
                Err(payload) => {
                    let detail = panic_text(payload.as_ref());
                    let line = format!("内部错误：{}任务线程崩溃（{detail}）", op.label);
                    // 崩溃原因也塞进日志正文：调用方长轮询到的是日志，让它跟 error
                    // 字段对得上，用户不用去翻应用的 stderr。
                    log_for_thread.push(&line);
                    let recovery = if op == OP_UNINSTALL || op == OP_UNINSTALL_RECOVERY {
                        "如有未结束维护，请先在模型商店核实并恢复维护。"
                    } else {
                        "请核实当前状态后重试。"
                    };
                    Err(format!(
                        "{line}。这是模型商店自身的缺陷，请把日志反馈给我们；{recovery}"
                    ))
                }
            });
        })
        .map_err(|error| format!("无法创建模型操作线程，请稍后重试：{error}"))?;

    Ok(json!({ "run_id": run_id }))
}

// ---------------------------------------------------------------- 只读查询

pub async fn list_cards() -> Result<Value, String> {
    tokio::task::spawn_blocking(|| {
        ModelManager::new(None, None)
            .list_store_cards()
            .map_err(|e| e.to_string())
            .and_then(|cards| serde_json::to_value(cards).map_err(|e| e.to_string()))
    })
    .await
    .map_err(|e| e.to_string())?
}

pub async fn library(service: Arc<ModelStoreService>) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || {
        if service.remote {
            let mut value = super::background::call("library", json!({}))?;
            // The host response carries freshly confirmed CMS observations;
            // keep them in this desktop too, including across later snapshot polls.
            value["linked_runtime_cards"] = json!(super::linked_models::RuntimeFacts::pending_cards(&value));
            let pending = service.observe_linked_runtime(&value)?;
            value["linked_runtime_pending"] = json!(pending);
            return Ok(value);
        }
        let mgr = ModelManager::new(None, None);
        // 先按实际容器状态纠正一次 running：设备可能是重启之后才起的应用，
        // 库里的 running 标记这时候是过期的。
        let corrected = mgr.reconcile_running_status();
        let mut models = db::list_store_models().map_err(|e| e.to_string())?;
        // MegaCube 融合：cmsd 认识的原生卡以管理面实况为准——商店对原生引擎
        // 历史是盲的（running 只靠脚本成败落库，systemd/外部启停永不纠正），
        // 这里每次读库顺手对齐；同时把重启计数/degraded/健康透出给行内徽标。
        // cmsd 不在场（普通机器）原样直行，行为与今天逐字节等价。
        let cmsd_states = crate::features::megacube::delegate::cmsd_states();
        // 管理面端点事实（网关+别名），融合修复用
        let cmsd_gateway = crate::features::megacube::delegate::gateway_endpoint();
        // 融合纠正单列：与容器纠正（corrected）分开，因为事实源不同——容器纠正的
        // toast 文案是「按实际容器状态」，而这里的事实源是 MegaCube 管理面实况，
        // 复用同一条文案会误导（真机实证：面板开着时这条 toast 把用户看糊涂了）。
        let mut cmsd_corrected: Vec<String> = Vec::new();
        if let Some(states) = cmsd_states.as_ref() {
            for rec in models.iter_mut() {
                let Some(state) = states.get(&rec.card_id) else {
                    continue;
                };
                if !state.managed {
                    continue;
                }
                // 只在 cmsd 有把握时动笔：running+探活通过 → true；
                // stopped/degraded → false；starting/未探活是过渡态，不动
                // （否则启用中读一次就被打成未运行，行会来回跳——真机踩过）。
                let cmsd_running = match (state.status.as_str(), state.healthy) {
                    ("running", Some(true)) => Some(true),
                    ("stopped" | "degraded", _) => Some(false),
                    _ => None,
                };
                let Some(cmsd_running) = cmsd_running else {
                    continue;
                };
                if rec.running != cmsd_running {
                    let _ = db::set_running(&rec.card_id, cmsd_running);
                    rec.running = cmsd_running;
                    cmsd_corrected.push(rec.card_name.clone());
                }
                // Repair only confirmed gateway and per-card model facts. The
                // global default alias may select a different card; an unknown
                // model identity must preserve the already saved connection.
                if let Some((gateway_url, _)) = cmsd_gateway.as_ref() {
                    let Some(real_model) = state.model_id.as_deref() else {
                        continue;
                    };
                    let stale_endpoint = rec.endpoint_url.as_deref() != Some(gateway_url.as_str())
                        || rec.endpoint_model.as_deref() != Some(real_model);
                    if stale_endpoint {
                        let _ = db::set_endpoint_info(
                            &rec.card_id,
                            Some(gateway_url.as_str()),
                            Some(real_model),
                        );
                        rec.endpoint_url = Some(gateway_url.clone());
                        rec.endpoint_model = Some(real_model.to_string());
                    }
                }
            }
        }
        let route_sync_error = super::linked_models::sync(None).err();
        let rows = db::list_store_models_light().map_err(|e| e.to_string())?;
        let mut payload = library_payload(rows);
        if let Some(states) = cmsd_states.as_ref() {
            attach_cmsd_states(&mut payload, states);
        }
        let runtime_payload = json!({
            "models": payload,
            "linked_runtime_scope": super::linked_models::runtime_scope(),
        });
        let mut runtime_pending = super::linked_models::RuntimeFacts::pending_cards(&runtime_payload);
        let link_sync_error = route_sync_error.or_else(|| {
            match service.observe_linked_runtime(&runtime_payload) {
                Ok(pending) => { runtime_pending = pending; None }
                Err(error) => Some(error),
            }
        });
        let (maintenance, maintenance_error) = match super::maintenance::summaries() {
            Ok(records) => (records, None),
            Err(error) => (Vec::new(), Some(error)),
        };
        let (incomplete, incomplete_error) = match super::incomplete::summaries() {
            Ok(items) => (items, None),
            Err(error) => (Vec::new(), Some(error)),
        };
        Ok(json!({
            "models": runtime_payload["models"],
            "linked_runtime_scope": runtime_payload["linked_runtime_scope"],
            "linked_runtime_pending": runtime_pending,
            "linked_runtime_cards": super::linked_models::RuntimeFacts::pending_cards(&runtime_payload),
            "maintenanceTransactions": maintenance,
            "maintenanceError": maintenance_error,
            "incompleteInstallations": incomplete,
            "incompleteError": incomplete_error,
            "corrected": corrected,
            "cmsd_corrected": cmsd_corrected,
            "link_sync_error": link_sync_error,
        }))
    })
    .await
    .map_err(|e| e.to_string())?
}

/// 把 cmsd 实况挂进每行记录的 `cmsd` 字段（前端徽标/状态展示用）。
/// 纯数据组装，与 IO 无关，单测直接覆盖。
fn attach_cmsd_states(
    payload: &mut Value,
    states: &std::collections::BTreeMap<
        String,
        crate::features::megacube::delegate::CmsdModelState,
    >,
) {
    let Some(items) = payload.as_array_mut() else {
        return;
    };
    for item in items.iter_mut() {
        let Some(card_id) = item.get("card_id").and_then(|v| v.as_str()) else {
            continue;
        };
        let Some(state) = states.get(card_id) else {
            continue;
        };
        if let Ok(v) = serde_json::to_value(state) {
            if let Some(obj) = item.as_object_mut() {
                obj.insert("cmsd".to_string(), v);
            }
        }
    }
}

pub async fn model_detail(card_id: String) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || {
        ModelManager::new(None, None)
            .get_model_detail(&card_id)
            .map_err(|e| e.to_string())
            .and_then(|d| {
                let default =
                    super::deploy_profiles::default_id(&d.record.card_id, &d.record.profiles);
                let mut value = serde_json::to_value(&d).map_err(|e| e.to_string())?;
                value["record"]["default_profile_id"] = json!(default);
                Ok(value)
            })
    })
    .await
    .map_err(|e| e.to_string())?
}

/// 仅列出已经过实体校验的引擎。数据库里的历史记录不能直接拿来显示，否则用户会
/// 看见“0 B、删不掉”的幽灵条目。
pub async fn installed_engines() -> Result<Value, String> {
    tokio::task::spawn_blocking(|| {
        serde_json::to_value(ModelManager::new(None, None).list_installed_engines())
            .map_err(|e| e.to_string())
    })
    .await
    .map_err(|e| e.to_string())?
}

pub async fn follow_deployment_context(
    service: Arc<ModelStoreService>,
    card_id: String,
    model_id: String,
) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || {
        super::linked_models::follow_deployment_context(&card_id, &model_id)?;
        service.notify_models_changed(&[model_id]);
        Ok(json!({"ok": true}))
    })
    .await
    .map_err(|e| e.to_string())?
}

/// 扫描用户配置的本地导入目录。结果只是“可能的权重资源”，不是模型归属结论；卡片
/// 脚本仍需在下载时核验它是否完整且确实属于当前模型。
pub async fn local_resources() -> Result<Value, String> {
    tokio::task::spawn_blocking(|| {
        let settings = config::load_settings();
        let candidates = import_scan::scan(&import_scan::roots(
            &settings.import_scan_dirs,
            &settings.models_path(),
            &settings.staging_path(),
        ));
        Ok(json!({
            "resources": candidates,
            "scan_dirs": import_scan::roots(
                &settings.import_scan_dirs,
                &settings.models_path(),
                &settings.staging_path(),
            ).iter().map(|path| path.to_string_lossy().to_string()).collect::<Vec<_>>(),
        }))
    })
    .await
    .map_err(|e| e.to_string())?
}

/// 启用前内存准入快照。前端据此决定是否需要给用户二次确认，真正开始启用时还会在
/// `start_lifecycle` 里复算一次，避免用户打开确认框后内存状态已变化。
pub async fn memory_admission(
    card_id: String,
    version_id: Option<String>,
) -> Result<Value, String> {
    // r43 P1-4：带 version_id 时按切换目标版本的记录评估（版本切换预检用），
    // 否则按 active 记录（启用/部署预检用）。
    tokio::task::spawn_blocking(move || {
        let eval = match version_id.as_deref() {
            Some(vid) => ModelManager::new(None, None).memory_admission_for_version(&card_id, vid),
            None => ModelManager::new(None, None).memory_admission(&card_id),
        };
        eval.map_err(|e| e.to_string())
            .and_then(|admission| serde_json::to_value(admission).map_err(|e| e.to_string()))
    })
    .await
    .map_err(|e| e.to_string())?
}

pub async fn check_card_update(card_id: String) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || {
        ModelManager::new(None, None)
            .check_card_update(&card_id)
            .map_err(|e| e.to_string())
    })
    .await
    .map_err(|e| e.to_string())?
}

/// 删除未被任何模型绑定的、且当前客户端有安全删除方式的引擎。
pub async fn delete_engine(
    service: &Arc<ModelStoreService>,
    engine_name: String,
    engine_version: Option<String>,
) -> Result<Value, String> {
    // 已下载的模型才会写入 store_models；正在下载或部署的卡片尚未必有一条
    // 可供 affected_models 查到的记录。若此时按“没有模型绑定”删除引擎，会让
    // 当前任务在末尾失去可复用的运行环境，下一步又得重下几十 GB。因此删除
    // 引擎必须同时避开所有活跃的模型商店操作。
    let cleanup_guard = service.active.try_begin_engine_cleanup()?;
    tokio::task::spawn_blocking(move || {
        // 由工作线程持有直到清理完成，即使调用方断开也不提前放开互斥。
        let _cleanup_guard = cleanup_guard;
        // r43 P1-7：ActiveCardOps 只互斥**本进程**的操作；下载/部署持有的是
        // 跨进程资源锁（wait_resource_operation_lock 共享档）——另一进程正在
        // 下载首个模型（尚未写 store_models 绑定行）时，本进程按"无绑定"删
        // 引擎会抽掉对方末尾要复用的运行环境（Ollama 形态甚至删整个数据目录）。
        // 取独占档：与所有持共享档的资源操作互斥，绑定复核+删除全程持锁。
        let _cross_process_resources = super::version_store::wait_resource_operation_lock(
            true,
            || false,
            || {
                let _ = std::io::Write::write(
                    &mut std::io::stderr(),
                    b"[engine-cleanup] waiting for cross-process resource operations...
",
                );
            },
        )
        .map_err(|e| e.to_string())?;
        let manager = ModelManager::new(None, None);
        let current = manager.list_installed_engines();
        let engine = current
            .iter()
            .find(|engine| engine.engine_name == engine_name && engine.engine_version == engine_version)
            .ok_or_else(|| "未找到可验证的本地引擎；它可能已被手动删除，请刷新列表。".to_string())?;
        if !engine.affected_models.is_empty() {
            return Err(format!(
                "该引擎仍被以下模型绑定，不能删除：{}。请先删除或重新部署这些模型。",
                engine.affected_models.join("、")
            ));
        }
        if !engine.cleanup_supported {
            return Err("该引擎已验证存在，但模型卡片尚未提供安全的独立清理方式；为避免误删模型文件，当前不能自动删除。".to_string());
        }
        manager
            .delete_engine(&engine_name, engine_version.as_deref())
            .map_err(|e| e.to_string())
            .and_then(|outcome| serde_json::to_value(outcome).map_err(|e| e.to_string()))
    })
    .await
    .map_err(|e| e.to_string())?
}

pub async fn startup_update_check(service: Arc<ModelStoreService>) -> Result<Value, String> {
    let enabled = config::startup_update_check_enabled()?;
    let report = service
        .startup_updates
        .once(super::available() && enabled, super::startup_updates::check)
        .await;
    serde_json::to_value(report).map_err(|e| e.to_string())
}

pub async fn versions(card_id: String, measure: Option<bool>) -> Result<Value, String> {
    let measure = measure.unwrap_or(false);
    tokio::task::spawn_blocking(move || {
        ModelManager::new(None, None)
            .versions(&card_id, measure)
            .map_err(|e| e.to_string())
    })
    .await
    .map_err(|e| e.to_string())?
}

pub fn start_version_action(
    service: &Arc<ModelStoreService>,
    card_id: String,
    action: String,
    version_id: String,
    memory_confirmed: bool,
) -> Result<Value, String> {
    if service.remote {
        return super::background::call(
            "version_action",
            json!({"card_id":card_id,"action":action,"version_id":version_id,"memory_confirmed":memory_confirmed}),
        );
    }
    let op = match action.as_str() {
        "stage" => CardOp {
            kind: "version_stage",
            label: "下载新版",
        },
        "switch" => CardOp {
            kind: "version_switch",
            label: "切换版本",
        },
        "delete" => CardOp {
            kind: "version_delete",
            label: "清理旧版",
        },
        _ => return Err("不支持的版本操作".into()),
    };
    // r42 P1-1 + r43 P1-4：switch 的 deploy 臂可能与部署脚本一样自行启动运行时
    // （同一理由见 start_deploy_checked 的守卫注释）——必须走与 enable/deploy 同一
    // 内存准入 + 启动槽位，且准入按**切换目标版本**的记录评估（active 记录属于
    // 即将被替换的版本）。stage/delete 不启动运行时，不占启动槽。guard 在
    // spawn_op 之前获取（锁序与 start_deploy_checked 一致：StartupGuard →
    // ActiveCardOps），并 move 进任务闭包持有全程。
    let startup_guard = (action == "switch")
        .then(|| prepare_model_start_for_version(&card_id, &version_id, memory_confirmed))
        .transpose()?;
    let card = card_id.clone();
    let queue = Arc::clone(&service.queue);
    spawn_op(service, &card_id, op, move |log, control, run_id| {
        let _startup_guard = startup_guard;
        if action == "stage" {
            let ticket = queue.enqueue(run_id);
            while !queue.try_start(ticket, config::load_settings().max_concurrent_downloads()) {
                if control.cancelled() {
                    return Err("已取消".into());
                }
                std::thread::sleep(Duration::from_millis(250));
            }
        }
        let mgr = ModelManager::new(None, None);
        let mut progress = |line: &str| log.push(line);
        match action.as_str() {
            "stage" => mgr.stage_version(&card, &version_id, &mut progress, control),
            "switch" => mgr.switch_version(&card, &version_id, &mut progress, control),
            _ => mgr.delete_version(&card, &version_id, &mut progress, control),
        }
        .map_err(|e| e.to_string())
    })
}

pub fn start_card_update(
    service: &Arc<ModelStoreService>,
    card_id: String,
    revision: String,
) -> Result<Value, String> {
    if service.remote {
        return super::background::call(
            "card_update",
            json!({"card_id":card_id,"revision":revision}),
        );
    }
    let card = card_id.clone();
    spawn_op(
        service,
        &card_id,
        CardOp {
            kind: "update",
            label: "更新卡片",
        },
        move |log, control, _| {
            ModelManager::new(None, None)
                .update_card(&card, &revision, &mut |line| log.push(line), control)
                .map_err(|e| e.to_string())
        },
    )
}
// ---------------------------------------------------------------- 长操作

/// 下载闸门（排队限流）：start_download / start_install_local 共用的「等到下载
/// 名额」闭包。
///
/// 取号要在真正开跑之前（所以在 job 线程一进来就构造）：用户点下载的先后顺序
/// 就是排队顺序，等装完引擎再取号会让装引擎快的插到慢的前面。
fn download_gate(
    queue: Arc<DownloadQueue>,
    log: Arc<RunLog>,
    control: script_runner::RunControl,
    run_id: &str,
) -> impl FnMut() -> Result<(), super::errors::AppError> {
    let ticket = queue.enqueue(run_id);
    let mut announced = false;
    move || -> Result<(), super::errors::AppError> {
        loop {
            if control.cancelled() {
                return Err(super::errors::AppError::new("已取消"));
            }
            // 每轮重读设置：用户可能在排队期间调大并发数，应当立刻生效
            let limit = config::load_settings().max_concurrent_downloads();
            if queue.try_start(ticket, limit) {
                return Ok(());
            }
            if !announced {
                announced = true;
                let ahead = queue.position(ticket);
                // 不说"引擎已就绪"——这条只是排队位的播报，引擎检查/安装
                // 在轮到之后才做（.48 真机反馈：排队时说就绪、轮到后装
                // 引擎失败，前后矛盾让用户以为是误判）。
                log.push(&format!(
                    "已进入下载队列（前面还有 {ahead} 个）\
                 ——可以放着不管，轮到它会自动检查引擎并开始下载"
                ));
            }
            std::thread::sleep(Duration::from_millis(500));
        }
    }
}

pub fn start_download(service: &Arc<ModelStoreService>, card_id: String) -> Result<Value, String> {
    start_download_with_id(service, card_id, None)
}

pub(super) fn start_download_with_id(
    service: &Arc<ModelStoreService>,
    card_id: String,
    requested_run_id: Option<String>,
) -> Result<Value, String> {
    if service.remote {
        return super::background::call("download", json!({"card_id":card_id}));
    }

    let card_for_job = card_id.clone();
    let queue = Arc::clone(&service.queue);

    spawn_op_with_id(
        service,
        &card_id,
        OP_DOWNLOAD,
        requested_run_id,
        move |log, control, run_id| {
            let mut gate =
                download_gate(Arc::clone(&queue), Arc::clone(log), control.clone(), run_id);
            let log_cb = Arc::clone(log);
            let mut cb = move |line: &str| log_cb.push(line);
            let manager = ModelManager::new(None, None);
            log.set_install_context(manager.install_context("download"))?;
            manager
                .download_from_store(&card_for_job, &mut cb, control, Some(&mut gate))
                .map(|r| serde_json::to_value(r).unwrap_or(Value::Null))
                .map_err(|e| e.to_string())
        },
    )
}

/// 本地卡片安装：执行已物化在 `<store_root>/cards/<card_id>/` 的卡（MCP 按 HF
/// 仓库元数据生成的驭光卡，或用户按同规格放好的卡）。
///
/// 与 start_download 共用 spawn_op / ActiveCardOps / 下载队列——socket 来的请求
/// 和界面点的按钮是同权客户端，撞的是同一把卡片互斥锁，排的是同一条队。
pub fn start_install_local(
    service: &Arc<ModelStoreService>,
    card_id: String,
) -> Result<Value, String> {
    start_install_local_with_id(service, card_id, None)
}

pub async fn preview_incomplete(
    service: &Arc<ModelStoreService>,
    run_id: String,
) -> Result<Value, String> {
    let service = Arc::clone(service);
    tokio::task::spawn_blocking(move || {
        if service.remote {
            return super::background::call("incomplete_preview", json!({"run_id":run_id}));
        }
        super::incomplete::preview(&run_id).map(|preview| preview.public)
    })
    .await
    .map_err(|error| error.to_string())?
}

/// 放弃一条失败的安装：未完成面板不再提示，重试入口一并关死。已下载的文件
/// 原样保留——断点续传是刻意设计，放弃撤的是提醒和重试凭据，不是数据。
pub async fn dismiss_incomplete(
    service: &Arc<ModelStoreService>,
    run_id: String,
) -> Result<Value, String> {
    let service = Arc::clone(service);
    tokio::task::spawn_blocking(move || {
        if service.remote {
            return super::background::call("incomplete_dismiss", json!({"run_id":run_id}));
        }
        dismiss_incomplete_local(&service, &run_id)
    })
    .await
    .map_err(|error| error.to_string())?
}

/// 放弃的本地执行路径。校验在 [`super::incomplete::ensure_dismissable`]（终结
/// 与否、种类、脚本进程已退出），改写走 SavedRun 的所有者这条唯一的落盘路——
/// 内存里还热着的 log 直接打标持久化；冷的（超出保留期、只剩磁盘文件）经
/// load_saved_run 原样装回再打标，不进内存表。
pub(super) fn dismiss_incomplete_local(
    service: &ModelStoreService,
    run_id: &str,
) -> Result<Value, String> {
    super::incomplete::ensure_dismissable(run_id)?;
    let log = match service.run(run_id) {
        Some(log) => log,
        None => {
            // 校验已经确认回执存在、受信且已终结；load_saved_run 在这条路上
            // 不会走到"补写中断结局"的分支。
            let path = config::data_dir().join("runs").join(format!("{run_id}.json"));
            Arc::new(load_saved_run(&path)?.ok_or("installation_record_unavailable")?)
        }
    };
    log.dismiss()?;
    Ok(json!({ "ok": true }))
}

pub fn retry_incomplete(
    service: &Arc<ModelStoreService>,
    run_id: String,
    expected_sha256: String,
) -> Result<Value, String> {
    if service.remote {
        return super::background::call(
            "incomplete_retry",
            json!({"run_id":run_id,"expected_sha256":expected_sha256}),
        );
    }
    let (card, kind, _) = super::incomplete::retry_request(&run_id)?;
    let configured = crate::features::megacube::local_authority_expected();
    match crate::features::megacube::Client::connect() {
        Ok(client) => {
            if !client.supports("model.install_retry.v1")? {
                return Err("installation_retry_capability_required".into());
            }
            let device = client.device()?;
            let device_id = device
                .get("device_id")
                .and_then(Value::as_str)
                .ok_or("installation_admission_unconfirmed")?;
            let accepted = client.retry_install(&card, &run_id, &expected_sha256)?;
            if accepted.get("error").is_some_and(|error| !error.is_null())
                || accepted
                    .get("http_status")
                    .and_then(Value::as_u64)
                    .is_some_and(|status| status >= 400)
            {
                return Err(format!("installation_admission_rejected: {accepted}"));
            }
            let task_id = accepted
                .get("task_id")
                .and_then(Value::as_str)
                .ok_or("installation_admission_unconfirmed")?;
            if task_id.is_empty()
                || task_id.len() > 100
                || !task_id
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || b"_-".contains(&byte))
            {
                return Err("installation_admission_unconfirmed".into());
            }
            let unconfirmed =
                || format!("installation_admission_unconfirmed: cmsd_task_id={task_id}");
            super::delegated_run::save(&client, task_id, &card, &kind, device_id)?;
            let detail = client.task(task_id).map_err(|_| unconfirmed())?;
            let store_run = detail
                .get("store_run_id")
                .and_then(Value::as_str)
                .ok_or_else(unconfirmed)?;
            if store_run != format!("cmsd-{task_id}")
                || detail["id"] != task_id
                || detail["card"] != card
                || detail["kind"] != "install"
            {
                return Err(unconfirmed());
            }
            return Ok(json!({"run_id":store_run,"cmsd_task_id":task_id}));
        }
        Err(error) if configured => return Err(format!("cmsd_unavailable: {error}")),
        Err(_) => {}
    }
    retry_incomplete_with_id(service, run_id, expected_sha256, None)
}

/// Fixed store-side delegation entry. The public UI chooses cmsd admission
/// first; cmsd calls this endpoint with its durable request correlation.
pub(super) fn retry_incomplete_with_id(
    service: &Arc<ModelStoreService>,
    run_id: String,
    expected_sha256: String,
    requested_run_id: Option<String>,
) -> Result<Value, String> {
    if expected_sha256.len() != 64
        || !expected_sha256
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err("installation_preview_changed".into());
    }
    let reference = super::incomplete::RetryReference {
        original_run_id: run_id.clone(),
        preview_sha256: expected_sha256.clone(),
    };
    if let Some(id) = requested_run_id.as_ref() {
        if let Some(log) = service.lookup_run(id)? {
            if log.retry_reference.lock().as_ref() != Some(&reference) {
                return Err("idempotency_conflict".into());
            }
            return Ok(json!({"run_id":id,"kind":log.kind,"card_id":log.card_id}));
        }
    }
    let (card, kind, context) = super::incomplete::request_identity(&run_id)?;
    let op = if kind == "install_local" {
        OP_INSTALL_LOCAL
    } else {
        OP_DOWNLOAD
    };
    let queue = Arc::clone(&service.queue);
    let response_card = card.clone();
    let response_kind = kind.clone();
    let mut accepted = spawn_op_with_metadata(
        service,
        &card,
        op,
        requested_run_id,
        Some((context, reference)),
        move |log, control, new_id| {
            log.push(&format!(
                "原运行 {run_id}；后台重新核对安装包，按原任务类型重试。"
            ));
            if control.cancelled() {
                return Err("已取消".into());
            }
            // Exclude only this server-generated run from the latest-task check.
            // Any other intervening task still supersedes the old installation.
            // Network package checks happen after durable acceptance, so the UI
            // already has a task ID and can observe/cancel the request.
            let preview = super::incomplete::preview_for_retry(&run_id, Some(new_id))?;
            super::incomplete::recheck(&run_id, &preview, &expected_sha256)?;
            if control.cancelled() {
                return Err("已取消".into());
            }
            let manager = ModelManager::new(None, None);
            if manager.install_context(&preview.kind) != preview.context {
                return Err("installation_storage_or_source_changed".into());
            }
            log.set_install_context(preview.context.clone())?;
            log.push("安装包与预览一致，保留已有文件并继续检查引擎和权重。");
            let mut gate =
                download_gate(Arc::clone(&queue), Arc::clone(log), control.clone(), new_id);
            let mut progress = |line: &str| log.push(line);
            let result = if preview.kind == "install_local" {
                manager.install_local_from_cards_dir_checked(
                    &preview.card_id,
                    &mut progress,
                    control,
                    Some(&mut gate),
                    Some(&preview.package_sha256),
                )
            } else {
                manager.download_from_store_checked(
                    &preview.card_id,
                    &mut progress,
                    control,
                    Some(&mut gate),
                    Some(&preview.package_sha256),
                )
            };
            result
                .map(|value| serde_json::to_value(value).unwrap_or(Value::Null))
                .map_err(|error| error.to_string())
        },
    )?;
    accepted["kind"] = json!(response_kind);
    accepted["card_id"] = json!(response_card);
    Ok(accepted)
}

pub(super) fn start_install_local_with_id(
    service: &Arc<ModelStoreService>,
    card_id: String,
    requested_run_id: Option<String>,
) -> Result<Value, String> {
    // Preserve exact accepted-run replays, but never create a new local task
    // for an already registered or retained physical version.
    if requested_run_id
        .as_ref()
        .and_then(|id| service.run(id))
        .is_none()
    {
        ModelManager::check_fresh_local_install(&card_id).map_err(|e| e.to_string())?;
    }
    let card_for_job = card_id.clone();
    let queue = Arc::clone(&service.queue);

    spawn_op_with_id(
        service,
        &card_id,
        OP_INSTALL_LOCAL,
        requested_run_id,
        move |log, control, run_id| {
            let mut gate =
                download_gate(Arc::clone(&queue), Arc::clone(log), control.clone(), run_id);
            let log_cb = Arc::clone(log);
            let mut cb = move |line: &str| log_cb.push(line);
            let manager = ModelManager::new(None, None);
            log.set_install_context(manager.install_context("install_local"))?;
            manager
                .install_local_from_cards_dir(&card_for_job, &mut cb, control, Some(&mut gate))
                .map(|r| serde_json::to_value(r).unwrap_or(Value::Null))
                .map_err(|e| e.to_string())
        },
    )
}

pub fn start_card_scripts(
    service: &Arc<ModelStoreService>,
    card_id: String,
    confirmed: bool,
) -> Result<Value, String> {
    if !confirmed {
        return Err("更新卡片脚本需要明确确认；不会下载或删除模型权重。".into());
    }
    if service.remote {
        return super::background::call(
            "card_scripts",
            json!({"card_id":card_id,"confirmed":confirmed}),
        );
    }

    let card = card_id.clone();
    let op = CardOp {
        kind: "card_scripts",
        label: "更新卡片脚本",
    };
    spawn_op(service, &card_id, op, move |log, control, _run_id| {
        let _startup_guard = super::runtime_guard::StartupGuard::acquire()?;
        log.push("检查模型运行状态及完整脚本包；不修改权重或引擎文件…");
        let changed = ModelManager::new(None, None)
            .change_card_scripts(&card, &control)
            .map_err(|e| e.to_string())?;
        log.push(if changed {
            "脚本包已安全保存。已部署模型请重新部署后再启用。"
        } else {
            "本地脚本包已是当前版本，没有修改。"
        });
        Ok(json!({ "changed": changed }))
    })
}

pub async fn start_deploy(
    service: &Arc<ModelStoreService>,
    card_id: String,
    profile_id: Option<String>,
    memory_confirmed: bool,
) -> Result<Value, String> {
    let service = Arc::clone(service);
    tokio::task::spawn_blocking(move || {
        if service.remote {
            return super::background::call(
                "deploy",
                json!({"card_id":card_id,"profile_id":profile_id,"memory_confirmed":memory_confirmed}),
            );
        }
        start_deploy_checked(&service, card_id, profile_id, memory_confirmed)
    })
    .await
    .map_err(|error| error.to_string())?
}

fn start_deploy_checked(
    service: &Arc<ModelStoreService>,
    card_id: String,
    profile_id: Option<String>,
    memory_confirmed: bool,
) -> Result<Value, String> {
    // Some deployment scripts start the runtime themselves. They must use the
    // same admission/startup slot as enable; otherwise deploy bypasses the gate.
    let startup_guard = prepare_model_start(&card_id, memory_confirmed)?;
    let card = card_id.clone();
    spawn_op(
        service,
        &card_id,
        OP_DEPLOY,
        move |log, control, _run_id| {
            let _startup_guard = startup_guard;
            let log_cb = Arc::clone(log);
            let mut cb = move |line: &str| log_cb.push(line);
            ModelManager::new(None, None)
                .deploy(&card, profile_id.as_deref(), &mut cb, control)
                .map(|_| Value::Null)
                .map_err(|e| e.to_string())
        },
    )
}

fn prepare_model_start(
    card_id: &str,
    memory_confirmed: bool,
) -> Result<super::runtime_guard::StartupGuard, String> {
    let guard = super::runtime_guard::StartupGuard::acquire()?;
    let admission = ModelManager::new(None, None)
        .memory_admission(card_id)
        .map_err(|e| e.to_string())?;
    if admission.requires_confirmation && !memory_confirmed {
        return Err(format!(
            "启动前需要确认内存风险：{} 请重新发起操作，确认后继续。",
            admission.message
        ));
    }
    Ok(guard)
}

/// r43 P1-4：版本切换的准入按目标版本评估（active 记录属于即将被替换的
/// 版本，画像可能远小于切换目标——拿它放行等于查错了对象）。
fn prepare_model_start_for_version(
    card_id: &str,
    version_id: &str,
    memory_confirmed: bool,
) -> Result<super::runtime_guard::StartupGuard, String> {
    let guard = super::runtime_guard::StartupGuard::acquire()?;
    let admission = ModelManager::new(None, None)
        .memory_admission_for_version(card_id, version_id)
        .map_err(|e| e.to_string())?;
    if admission.requires_confirmation && !memory_confirmed {
        return Err(format!(
            "启动前需要确认内存风险（按切换目标版本评估）：{} 请重新发起操作，确认后继续。",
            admission.message
        ));
    }
    Ok(guard)
}

/// 启用 / 停用 / 卸载三个动作形状一样，只差调哪个方法，合并成一个入口。
///
/// `purge_weights` 只对卸载有意义，而且**缺省是保留**——不可逆的那一侧绝不能靠
/// "没写"命中。
pub async fn start_lifecycle(
    service: &Arc<ModelStoreService>,
    card_id: String,
    action: String,
    purge_weights: bool,
    memory_confirmed: bool,
) -> Result<Value, String> {
    let service = Arc::clone(service);
    tokio::task::spawn_blocking(move || {
        start_lifecycle_checked(&service, card_id, action, purge_weights, memory_confirmed)
    })
    .await
    .map_err(|error| error.to_string())?
}

fn start_lifecycle_checked(
    service: &Arc<ModelStoreService>,
    card_id: String,
    action: String,
    purge_weights: bool,
    memory_confirmed: bool,
) -> Result<Value, String> {
    if service.remote {
        return super::background::call(
            "lifecycle",
            json!({"card_id":card_id,"action":action,"purge_weights":purge_weights,"memory_confirmed":memory_confirmed}),
        );
    }

    let op = match action.as_str() {
        "enable" => OP_ENABLE,
        "stop" => OP_STOP,
        "uninstall" => OP_UNINSTALL,
        "recover_maintenance" => OP_UNINSTALL_RECOVERY,
        other => return Err(format!("未知操作：{other}")),
    };

    // Acquire before reading memory/running state, and carry into the job. A
    // per-card mutex cannot stop two different cards both observing zero models.
    let startup_guard = if op == OP_ENABLE {
        Some(prepare_model_start(&card_id, memory_confirmed)?)
    } else {
        None
    };

    // 正在跑的模型不许直接卸载，先停用。
    //
    // 挡的不是误点，是**无主的运行实例**：卡片的 uninstall.sh 里停服务那步失败时，
    // 有的卡会"继续清理权重"（驭光那张的原文就是
    // `stop.sh || echo "注意：停服务那步没完全成功，继续清理权重"`）。于是权重删了、
    // purge 那条路连库记录也删了，而进程还在跑。记录一删 `container_name` 跟着没了
    // ——那是判断"某个容器是不是本应用起的"唯一可靠依据（见 enable 里那段注释），
    // 此后 `reconcile_running_status` 再也认不出它：端口和显存占着，界面上停不掉。
    //
    // 放在这一层而不是 `ModelManager::uninstall` 里面：那是**机制**，要保持可组合
    // （将来做"自动停用再卸载"时正需要直接调它，而且它现有的语义就包含"卸载会清掉
    // running / container_name / endpoint_url"，有测试钉着）。"运行中该不该拦"是
    // **策略**，属于最外层接口——本模块既有的分层就是这么定的：`purge_weights` 的
    // 默认值也只在最外层定，方法签名里留必填参数（见 lifecycle.rs 里 uninstall 的
    // 文档注释）。
    //
    // r21 P2-1：核验本体挪进了 job 闭包顶部（下方 uninstall 臂）。原先它做在
    // `spawn_op` 之前——与取锁（StartupGuard 的 flock + ActiveCardOps 的卡片
    // 互斥）之间留着一个窗口：并发的 enable 恰好卡在两步之间完成时，解锁后的
    // 卸载直跑清理脚本，活实例脚下删权重（正是上面注释自列的 `stop || echo
    // 继续` 卡类）。挪到锁内复跑，分层语义不变（策略仍在最外层接口，只是换到
    // 持锁后执行）；失败同样如实报给 run log，任务卡上看得见。

    let card = card_id.clone();
    let service_for_job = Arc::clone(service);
    spawn_op(service, &card_id, op, move |log, control, _run_id| {
        let _startup_guard = startup_guard;
        let log_cb = Arc::clone(log);
        let mut cb = move |line: &str| log_cb.push(line);
        let mgr = ModelManager::new(None, None);
        match action.as_str() {
            "enable" => {
                // Bind legacy rows while the old endpoint still exists. The second
                // pass commits the actual new route even if the view was unmounted.
                let changed = super::linked_models::sync_routes(Some(&card))?;
                service_for_job.notify_models_changed(&changed);
                mgr.enable(&card, &mut cb, control)
                    .map_err(|e| e.to_string())?;
                let link_sync_error = service_for_job.refresh_enabled_model(&card).err();
                Ok(json!({"link_sync_error": link_sync_error}))
            }
            "stop" => mgr
                .stop(&card, &mut cb, control)
                .map(|_| Value::Null)
                .map_err(|e| e.to_string()),
            "recover_maintenance" => super::maintenance::recover(&card, &control, &mut cb),
            _ => {
                // 三重停验（r21 P2-1 挪进锁内，见上方注释）：此刻 StartupGuard 的
                // flock 与 ActiveCardOps 的卡片互斥都已持有，并发 enable 不可能再
                // 从这条核验和清理脚本之间挤过去。DB 读错必须显式失败（静默放行 =
                // 带病卸载）；running 检查是真实核验——stop 半成功时标志是 false
                // 而进程还活，只信标志会在活进程脚下卸载并抹掉 container_name
                // （reconcile 反向认领靠它，清掉即永久失控）。
                let rec = db::get_store_model(&card)
                    .map_err(|e| format!("读取模型记录失败，已取消卸载：{e}"))?
                    .ok_or_else(|| "该模型尚未下载".to_string())?;
                mgr.require_actually_stopped(&rec, "卸载")
                    .map_err(|e| format!("「{}」{}", rec.card_name, e))?;
                mgr.uninstall(&card, purge_weights, &mut cb, control)
                    .map(|outcome| serde_json::to_value(outcome).unwrap_or(Value::Null))
                    .map_err(|e| e.to_string())
            }
        }
    })
}

// ---------------------------------------------------------------- 运行日志

/// 读一段运行日志。游标 `from` 在调用方手里——断线重连不丢行也不重复，这是整条
/// 长操作链路能做到「界面切走再切回来也不影响」的原因。
///
/// `wait` 让这里在没有新行时最多挂 25 秒再返回，所以前端的轮询循环不是忙等。
pub async fn run_log(
    service: Arc<ModelStoreService>,
    run_id: String,
    from: usize,
    wait: Option<u64>,
    from_byte: Option<usize>,
) -> Result<Value, String> {
    run_log_page(
        service,
        run_id,
        from,
        wait,
        super::log_page::Options {
            from_byte: from_byte.unwrap_or(0),
            ..Default::default()
        },
    )
    .await
}

pub(super) async fn run_log_page(
    service: Arc<ModelStoreService>,
    run_id: String,
    from: usize,
    wait: Option<u64>,
    options: super::log_page::Options,
) -> Result<Value, String> {
    let options = options.validate()?;
    if service.remote {
        return tokio::task::spawn_blocking(move || {
            super::background::call("log", json!({"run_id":run_id,"from":from,"wait":wait,"from_byte":options.from_byte,"limit":options.limit,"tail":options.tail,"preview_chars":options.preview_chars}))
        })
        .await
        .map_err(|e| e.to_string())?;
    }

    let log = match service.run(&run_id) {
        Some(log) => log,
        None => {
            let pending_id = run_id.clone();
            let response =
                tokio::task::spawn_blocking(move || {
                    match super::delegated_run::parent(&pending_id) {
                        Ok(Some(parent)) => Ok(Some(
                            super::delegated_run::log(&parent.task, from)
                                .unwrap_or_else(|_| super::delegated_run::unconfirmed(from)),
                        )),
                        Ok(None) => Ok(None),
                        Err(_) if super::delegated_run::known(&pending_id) => {
                            Ok(Some(super::delegated_run::unconfirmed(from)))
                        }
                        Err(error) => Err(error),
                    }
                })
                .await
                .map_err(|error| error.to_string())??;
            if let Some(response) = response {
                if response["backend_pending"] == true || response["unconfirmed"] == true {
                    tokio::time::sleep(Duration::from_secs(wait.unwrap_or(1).min(1))).await;
                }
                return Ok(response);
            }
            return Err("没有这个运行记录（可能已过期）".into());
        }
    };

    let wait = wait
        .map(|s| Duration::from_secs(s.min(LOG_WAIT.as_secs())))
        .unwrap_or(LOG_WAIT);
    let deadline = Instant::now() + wait;
    loop {
        // **先登记成等待者，再取快照**，顺序不能反。
        //
        // `RunLog::push` 用的是 `notify_waiters()`，它只唤醒**此刻已经登记**的等待
        // 者、不预存许可。原先的写法是先 snapshot、再 `notified()`，于是 worker 在
        // 这两步之间 push 的那一行唤醒会丢：本次长轮询拿不到新行，又等不到唤醒，
        // 只能挂满剩余的 `LOG_WAIT`（25 秒）才靠超时重查。
        //
        // 这条恰好压在交互提示（`awaiting_input`）的路径上——脚本问一句话，用户
        // 最坏要等 25 秒才看见。`enable()` 是 tokio 给这个场景的惯用法：它把
        // waiter 立刻挂上去而不 await，所以「登记 → 快照 → 等待」三步之间不存在
        // 能丢唤醒的窗口。
        let notified = log.notify.notified();
        let mut notified = std::pin::pin!(notified);
        notified.as_mut().enable();

        let (page, outcome) = log.snapshot(from, options)?;
        // 有新行、或者已经结束，就立刻返回；否则挂着等唤醒
        if !page.lines.is_empty()
            || page.fragment.is_some()
            || outcome.is_some()
            || Instant::now() >= deadline
        {
            let (done, ok, result, error) = match outcome {
                None => (false, false, Value::Null, Value::Null),
                Some(Ok(v)) => (true, true, v, Value::Null),
                Some(Err(e)) => (true, false, Value::Null, Value::String(e)),
            };
            let (result, result_truncated) = super::log_page::bounded_result(result);
            let (error, error_truncated) = match error.as_str() {
                Some(text) => {
                    let (summary, truncated) = super::log_page::summary(text);
                    (json!(summary), truncated)
                }
                None => (error, false),
            };
            let mut value = serde_json::to_value(page).map_err(|e| e.to_string())?;
            let metadata = json!({
                "done": done,
                "retained_from": *log.line_offset.lock(),
                "history_truncated": *log.line_offset.lock() > 0,
                "ok": ok,
                "cancelled": *log.cancelled.lock(),
                // 「现在还有没有提示在等人答」。调用方从日志行里解析提示，而日志
                // 是可回放的——没有这个标志，界面切走再回来就会把早已答过的提示
                // 再弹一次，用户填的答案落进一个没人来取的槽，下一次真正的提问
                // 会被这份陈答案顶掉。
                "awaiting_input": *log.awaiting_prompt.lock(),
                // 「这次运行声明过保留原生进程吗」。结构化下发而不是让前端从日志
                // 行里认：回放一裁剪，行可以丢，声明不能丢——丢了它，残存进程就
                // 没有常驻的停用入口了。
                "retained_native_process": *log.retained_native.lock(),
                "result": result,
                "error": error,
                "result_truncated": result_truncated,
                "error_truncated": error_truncated,
            });
            value
                .as_object_mut()
                .ok_or("invalid_log_page")?
                .extend(metadata.as_object().ok_or("invalid_log_metadata")?.clone());
            return Ok(value);
        }
        let _ = tokio::time::timeout(deadline.saturating_duration_since(Instant::now()), notified)
            .await;
    }
}

/// 在文件管理器里打开运行日志目录。
///
/// 原始日志只在本机查看；复制/导出另走更严格脱敏后的有界快照。
pub fn reveal_run_logs_dir() -> Result<Value, String> {
    let dir = run_log_store::logs_dir();
    {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new()
            .mode(0o700)
            .recursive(true)
            .create(&dir)
    }
    .map_err(|e| format!("创建日志目录失败：{e}"))?;
    crate::platform::os::reveal_target(&dir)?;
    Ok(json!({ "path": dir.to_string_lossy() }))
}

/// 导出一次运行的日志（比落盘那份多过一道脱敏，见 `run_log_store`）。
pub fn export_run_log(run_id: String) -> Result<Value, String> {
    let (path, text) = run_log_store::export_run_log(&run_id)?;
    Ok(json!({ "path": path.to_string_lossy(), "text": text }))
}

pub fn cancel_run(service: &Arc<ModelStoreService>, run_id: String) -> Result<Value, String> {
    if service.remote {
        return super::background::call("cancel", json!({"run_id":run_id}));
    }

    if service.run(&run_id).is_none() {
        if let Some(parent) = super::delegated_run::parent(&run_id)? {
            if parent.task["status"] == "cancelled" {
                return Ok(json!({"ok":true,"cancelled":true}));
            }
            if !matches!(parent.task["status"].as_str(), Some("running" | "queued")) {
                return Err("installation_cancel_too_late".into());
            }
            let response = parent.client.cancel_task(&parent.task_id)?;
            if response.get("error").is_some_and(|error| !error.is_null())
                || response
                    .get("http_status")
                    .and_then(Value::as_u64)
                    .is_some_and(|status| status >= 400)
            {
                return Err(format!("installation_cancel_unconfirmed: {response}"));
            }
            return Ok(json!({"ok":true,"cmsd_task_id":parent.task_id,"status":"cancelling"}));
        }
        return Err("installation_run_unknown".into());
    }

    service.registry.cancel(&run_id);
    Ok(json!({ "ok": true }))
}

/// 这张卡（不给 `card_id` 就是全部卡）上还在跑的长操作。
///
/// **界面靠它把任务卡接回去。** 模型商店的视图是条件挂载的，切一下视图整棵树
/// 就卸载，任务态和 run_id 索引跟着一起没；而一次下载要跑几十分钟到十几小时。
/// 没有这条查询，用户切走再回来就等于把这次运行丢了：看不到进度、点不了取消、
/// 脚本请求输入也没人答（600 秒后被 `PROMPT_TIMEOUT` 判死），再点一次下载还会
/// 撞上卡片互斥，被界面渲染成一张说不清原因的失败卡。
///
/// 回的是登记表的快照，不含日志——拿到 run_id 之后照常走 `run_log`，游标在调用
/// 方手里，前半段一行都不会丢。
pub fn active_runs(
    service: &Arc<ModelStoreService>,
    card_id: Option<String>,
) -> Result<Value, String> {
    if service.remote {
        return super::background::call("active", json!({"card_id":card_id}));
    }

    let runs = match card_id.as_deref() {
        Some(id) => service.active.run_for_card(id).into_iter().collect(),
        None => service.active.active_runs(),
    };
    let mut runs: Vec<Value> = runs
        .into_iter()
        .map(serde_json::to_value)
        .collect::<Result<_, _>>()
        .map_err(|error| error.to_string())?;
    let (pending, delegated_error) = super::delegated_run::pending(card_id.as_deref());
    for run in pending {
        if !runs
            .iter()
            .any(|existing| existing["run_id"] == run["run_id"])
        {
            runs.push(run);
        }
    }
    // A receiver can materialize while the parent lookup is in flight. Refresh
    // the in-memory snapshot without holding its lock over network I/O.
    for run in service
        .active
        .active_runs()
        .into_iter()
        .filter(|run| card_id.as_ref().is_none_or(|id| id == &run.card_id))
    {
        let value = serde_json::to_value(run).map_err(|error| error.to_string())?;
        if !runs
            .iter()
            .any(|existing| existing["run_id"] == value["run_id"])
        {
            runs.push(value);
        }
    }
    Ok(json!({ "runs": runs,"delegatedError":delegated_error }))
}

// ---------------------------------------------------------------- 任务快照

/// 一次运行透给本地下载服务（local_service 的 /v1/tasks）的快照。
///
/// 字段跟应答 JSON 一一对应。`lines` 只在单任务详情里带（末尾至多 N 行），
/// 列表接口恒为空——几百个任务各自拖几百行日志，列表应答会胖得没法看。
pub struct TaskSnapshot {
    pub run_id: String,
    pub card_id: String,
    pub kind: &'static str,
    /// running / succeeded / failed / cancelled（取消单列，不等同失败——
    /// RunLog 本来就有 cancelled 标记）。
    pub status: &'static str,
    pub created_at: String,
    /// 最后一条 LMM_PROGRESS_BYTES 约定行解析出的（已下载, 总量）；没有过进度行为 None。
    pub progress: Option<(u64, u64)>,
    /// 日志最后一行（任务「现在在干什么」的一眼答案）；还没有任何输出为 None。
    pub last_line: Option<String>,
    /// 失败/取消时的中文错误；成功或还在跑为 None。
    pub error: Option<String>,
    pub lines: Vec<String>,
    pub line_offset: usize,
    pub lines_truncated: bool,
    pub last_line_truncated: bool,
    pub error_truncated: bool,
    /// 排序键（新的在前），不进 JSON。
    seq: u64,
}

/// 从日志尾部往前找最后一条 `LMM_PROGRESS_BYTES=done/total` 约定行。
///
/// 判据跟前端 `model-store-logic.js` 的正则逐字对齐：`LMM_PROGRESS_BYTES=(\d+)/(\d+)`
/// 出现在行内任意位置都算（下载段的行带着 `[下载模型] ` 前缀）。
fn parse_progress(lines: &[String]) -> Option<(u64, u64)> {
    const MARK: &str = "LMM_PROGRESS_BYTES=";
    for line in lines.iter().rev() {
        let Some(at) = line.find(MARK) else {
            continue;
        };
        let rest = &line[at + MARK.len()..];
        // 约定行的两个数字都紧跟标记，逐段取前导数字——跟 JS 的 \d+ 贪心匹配同语义
        let done_len = rest.chars().take_while(|c| c.is_ascii_digit()).count();
        if done_len == 0 {
            continue;
        }
        let Some(rest_total) = rest[done_len..].strip_prefix('/') else {
            continue;
        };
        let total_len = rest_total
            .chars()
            .take_while(|c| c.is_ascii_digit())
            .count();
        if total_len == 0 {
            continue;
        }
        // 数字段都是 ASCII，字符数 == 字节数，按字节切片安全
        let done = rest[..done_len].parse::<u64>().ok()?;
        let total = rest_total[..total_len].parse::<u64>().ok()?;
        return Some((done, total));
    }
    None
}

impl RunLog {
    fn to_task_snapshot(&self, run_id: &str, tail: usize) -> TaskSnapshot {
        let (progress, last_line, lines, line_offset, lines_truncated, last_line_truncated) = {
            let lines = self.lines.lock();
            let tail_page = if tail > 0 {
                super::log_page::page(
                    &lines,
                    0,
                    super::log_page::Options {
                        tail: Some(tail.min(500)),
                        preview_chars: Some(1000),
                        ..Default::default()
                    },
                )
                .ok()
            } else {
                None
            };
            let offset = *self.line_offset.lock();
            let tail_offset = offset + tail_page.as_ref().map_or(lines.len(), |page| page.from);
            let lines_truncated = offset > 0
                || tail_page
                    .as_ref()
                    .is_some_and(|page| !page.truncated_lines.is_empty());
            let tail_lines = tail_page.map_or_else(Vec::new, |page| page.lines);
            let last = lines.last().map(|line| super::log_page::summary(line));
            (
                (*self.last_progress.lock()).or_else(|| parse_progress(&lines)),
                last.as_ref().map(|(text, _)| text.clone()),
                tail_lines,
                tail_offset,
                lines_truncated,
                last.is_some_and(|(_, truncated)| truncated),
            )
        };
        let outcome = self.outcome.lock().clone();
        let cancelled = *self.cancelled.lock();
        let (status, error) = match &outcome {
            None => ("running", None),
            Some(Ok(_)) => ("succeeded", None),
            // 取消也要把那句「已取消」带出去：它解释的是"为什么停"，不是"炸了"
            Some(Err(e)) if cancelled => ("cancelled", Some(super::log_page::summary(e).0)),
            Some(Err(e)) => ("failed", Some(super::log_page::summary(e).0)),
        };
        TaskSnapshot {
            run_id: run_id.to_string(),
            card_id: self.card_id.clone(),
            kind: self.kind,
            status,
            created_at: self.created_at.clone(),
            progress,
            last_line,
            error,
            lines_truncated,
            last_line_truncated,
            error_truncated: outcome.as_ref().is_some_and(|result| {
                result
                    .as_ref()
                    .err()
                    .is_some_and(|error| super::log_page::summary(error).1)
            }),
            lines,
            line_offset,
            seq: self.seq,
        }
    }
}

impl ModelStoreService {
    /// 全部运行的快照（新的在前）。本地下载服务 /v1/tasks 的数据源。
    pub(crate) fn task_snapshots(&self) -> Vec<TaskSnapshot> {
        let mut out: Vec<TaskSnapshot> = self
            .runs
            .lock()
            .iter()
            .map(|(run_id, log)| log.to_task_snapshot(run_id, 0))
            .collect();
        out.sort_by(|a, b| b.seq.cmp(&a.seq));
        out
    }

    /// 单任务快照，带末尾至多 `tail` 行日志。没有这个运行记录返回 None
    /// （可能已过期被清扫——运行记录只保留 600 秒）。
    pub(crate) fn task_snapshot(&self, run_id: &str, tail: usize) -> Option<TaskSnapshot> {
        let log = self.run(run_id)?;
        Some(log.to_task_snapshot(run_id, tail))
    }
}

/// 回答脚本请求的那次输入。
///
/// `answer` 传 `None` 表示用户点了取消——会把这次运行整个中断，而不是拿一个空串
/// 继续往下跑（双机部署里那就是拿空的从机 IP 去连）。
pub fn answer_run(
    service: &Arc<ModelStoreService>,
    run_id: String,
    answer: Option<String>,
) -> Result<Value, String> {
    if service.remote {
        return super::background::call("answer", json!({"run_id":run_id,"answer":answer}));
    }

    service.registry.answer_prompt(&run_id, answer);
    // 立刻落下"没人在等了"。等脚本打出下一行再落的话，用户答完马上切走、再切
    // 回来的那一瞬间，回放会把这条已经答过的提示又弹一次。
    if let Some(log) = service.run(&run_id) {
        log.answered();
    }
    Ok(json!({ "ok": true }))
}

// ---------------------------------------------------------------- API Key

pub fn api_keys(card_id: String) -> Result<Value, String> {
    db::list_api_keys_for_card(&card_id)
        .map_err(|e| e.to_string())
        .and_then(|keys| serde_json::to_value(keys).map_err(|e| e.to_string()))
}

pub fn create_api_key(card_id: String, label: Option<String>) -> Result<Value, String> {
    db::add_api_key(&card_id, label.as_deref())
        .map_err(|e| e.to_string())
        .and_then(|key| serde_json::to_value(key).map_err(|e| e.to_string()))
}

// 这里原来是**测试商店开关**：一个口令哈希、`test_store_state` /
// `unlock_test_store` / `disable_test_store` 三个操作，以及把开关写进
// `config.json` 的 `set_test_store_enabled`。整组连同内置测试卡片一起在
// 2026-09-30 拿掉了，前端那个「连点标题 7 次弹口令框」的隐藏入口也一起没了。
//
// 那个口令从来只是防呆（哈希编译在二进制里，逆向拿得到），真正的边界是**卡片
// 根本不进发布包**——现在是这样了。

// ------------------------------------------------------ 启动时回收残留进程

/// 应用启动时收掉上次没收干净的脚本进程，报告留着给界面取。
///
/// **必须在启动阶段真的被调用**——这段防御原先写了、测了、没接线（`orphans` 模块
/// 全仓零生产调用点），等于没有。它要防的事故是：应用被强杀或崩溃之后，`hf download`
/// 之类的子进程还活着并占着权重目录的文件锁，用户下一次下载会**一直卡住且没有任何
/// 提示**——真机上这么僵持过 16 小时。
///
/// 同步跑而不是丢后台：这一步只是读一个登记文件、对着 `/proc` 核对几条记录再发信号，
/// 代价是毫秒级；而放后台就会跟前端 `take` 抢时序，用户可能永远看不到那条提示。
pub fn run_startup_cleanup(service: &Arc<ModelStoreService>) {
    if service.remote {
        return;
    }
    // r43 P1-3：老库内置卡一次性迁移必须先于孤儿清理——迁移后行才有脚本副本，
    // 用户随后的停用/卸载不再撞「本地路径被拒」死锁。幂等：全套已补齐的行
    // 零成本跳过；迁移失败不阻断启动（行保持原状，与迁移前行为一致）。
    let migrated = ModelManager::migrate_legacy_builtin_scripts();
    if !migrated.is_empty() {
        eprintln!(
            "[model-store] 老库内置卡迁移完成：{} 张（{}）",
            migrated.len(),
            migrated.join("、")
        );
    }
    let report = super::orphans::cleanup_on_startup();
    if report.is_empty() {
        return;
    }
    eprintln!(
        "[model-store] 启动清理：收掉 {} 个残留进程，{} 个以 root 运行收不掉",
        report.killed.len(),
        report.skipped_root.len()
    );
    service.startup_cleanup.set(report);
}

/// 取走启动清理的报告。**取一次就没了**——否则用户每切一次视图就被同一条提示弹一遍。
pub fn take_startup_cleanup(service: &Arc<ModelStoreService>) -> Result<Value, String> {
    if service.remote {
        return super::background::call("cleanup", json!({}));
    }

    Ok(match service.startup_cleanup.take() {
        Some(r) => json!({ "killed": r.killed, "skipped_root": r.skipped_root }),
        None => json!({ "killed": [], "skipped_root": [] }),
    })
}

// ------------------------------------------------------ 权重目录与导入扫描

/// 权重放在哪、下载前去哪找已经在本机的副本。
///
/// 这两件事要能看、能改，理由是同一个：**权重目录一旦跟用户实际存放的位置对
/// 不上，下载会白跑几十个小时，而界面上看不出任何异常**。2026-09-20 真机上就
/// 是这样——用户把 124GB 的权重放在 `~/models`（LMM 作为独立应用时的默认目录），
/// 而原生化时默认改成了 `~/.pinvou3/model-store/models`，他只看到进度条在涨。
///
/// 返回里除了可编辑的 `import_scan_dirs`，还给出 `scan_dirs`——**实际会扫的完整
/// 列表**，含内置位置和当前挂着的 U 盘。用户真正要问的是"到底找了哪些地方"，
/// 只回一个补充列表答不了这个问题。
/// 设备管理删除守卫的"已安装卡片实际引用的权重路径"集合（可失败版）。
///
/// 就地复用不搬家：卡片脚本按约定把 `$LMM_MODELS_DIR/<HF 仓库名>` 软链到扫描
/// 目录原位（import_scan 候选约定），宿主不记录落点——2026-11-04 评审 P2 的
/// 两条腿（升级空历史、历史淘汰不查引用）都源于"目录级保护超集覆盖不到存量
/// 引用"，这里从**在册模型 + 约定候选路径**恢复实际引用：枚举全部在册记录的
/// 约定权重候选（当前 models_dir 与历史根下各一份，换目录不搬权重），exists()
/// 过滤后 canonicalize 解析软链得到真实落点。卡片目录（data_dir/cards/<id>）
/// 不在集合里——它恒在商店数据根之下，路径臂本已覆盖。另并入卡片脚本自己
/// 落盘的 .env MODEL_PATH 权威路径（改名目录/零痕迹旧根的兜底，见
/// card_env_model_paths 的文档）。
///
/// fail-closed：settings/DB 读不了 → Err（与商店名单查询同口径，破坏性移除
/// 路径不得当未命中放行）。与 residual_paths 同一把 exists() 尺子但方向相反：
/// 提示文案宁可漏报，保护集合宁可多报。
pub(crate) fn installed_weight_paths_checked() -> Result<Vec<std::path::PathBuf>, String> {
    installed_weight_paths_for_removal_checked(true)
}

/// Keep main's legacy reference guards for every removal. The card-update
/// records add protection only for physical weight deletion, so unregistering
/// an otherwise removable external record does not acquire a new restriction.
pub(crate) fn installed_weight_paths_for_removal_checked(
    include_persisted_roots: bool,
) -> Result<Vec<std::path::PathBuf>, String> {
    let records = db::list_store_models().map_err(|e| format!("读取在册模型失败：{e}"))?;
    // 候选根取商店权重根全集（当前+历史 models_dir、导入目录及其历史、staging、
    // 内置家目录根）：旧版本允许一次设置变更同时换权重目录并移除扫描目录且
    // 不留任何历史（2026-11-04 评审 P2），此时旧默认根（~/models 等）上的
    // 在册引用只有内置根能恢复；集合同源 collect_weights_roots，与守卫的
    // 目录级保护同口径。
    let roots = crate::features::model_store::store_weights_roots_checked()?;
    let mut out: Vec<std::path::PathBuf> = Vec::new();
    for rec in &records {
        // 与 lifecycle::candidate_paths 同一候选约定（权重两臂）：来源仓库名
        // 末段与用户模型名，都过 safe_path_component 防逃逸。
        let mut names: Vec<String> = Vec::new();
        if let Some(repo) = rec.source_repo_id.as_deref() {
            if let Some(base) = repo.rsplit('/').next() {
                if let Ok(safe) = config::safe_path_component(base) {
                    names.push(safe);
                }
            }
        }
        if let Ok(safe) = config::safe_path_component(&rec.model_name) {
            names.push(safe);
        }
        for root in &roots {
            for name in &names {
                let candidate = root.join(name);
                if candidate.exists() {
                    let resolved = candidate.canonicalize().unwrap_or(candidate);
                    if !out.contains(&resolved) {
                        out.push(resolved);
                    }
                }
            }
        }
    }
    // The card-update PR persists authoritative roots that can outlive the
    // bounded configuration history. Consume those references directly instead
    // of attempting to reconstruct them only from current settings and names.
    if include_persisted_roots {
        let mut registered = Vec::new();
        for record in &records {
            if let Some(root) = &record.models_root {
                registered.push(std::path::PathBuf::from(root));
            }
            registered.extend(super::version_store::download_paths(&record.card_id)?);
        }
        for version in super::version_store::list(None)? {
            registered.extend(version.models_root.map(std::path::PathBuf::from));
            registered.extend(version.resource_dir.map(std::path::PathBuf::from));
        }
        registered.extend(super::version_store::active_download_roots()?);
        for path in registered {
            if !path.is_absolute() {
                return Err("model_store_weight_path_invalid".into());
            }
            // Keep a not-yet-created download target protected as well. A path
            // resolution failure must not discard a registered reference.
            let resolved = match path.canonicalize() {
                Ok(resolved) => resolved,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => path,
                Err(error) => return Err(format!("model_store_weight_path_unavailable: {error}")),
            };
            if !out.contains(&resolved) {
                out.push(resolved);
            }
        }
    }
    // .env MODEL_PATH 恢复腿（2026-11-08 评审 P2-3 处方一）：约定候选覆盖不
    // 了的「改名目录/零痕迹旧根」由卡片脚本自己落盘的权威路径兜底（存在性
    // 过滤 + canonicalize 与上方同一把尺）；见 card_env_model_paths 的文档。
    for resolved in card_env_model_paths(&records) {
        if !out.contains(&resolved) {
            out.push(resolved);
        }
    }
    Ok(out)
}

/// 设备管理删除守卫的「在册卡片权重目录约定名」集合（可失败版）。
///
/// 与 [`installed_weight_paths_checked`] 互补：后者按「已知根 × 约定名」恢复
/// 引用路径，只能覆盖根集合枚举得到的位置——旧版本允许在保存存储设置时换根
/// 并同时移除扫描目录且不留任何历史（2026-11-04 评审 P2），那样的旧自定义根
/// 升级后不在任何历史/内置根中，**零痕迹**。本函数返回名字集合，只依赖在册
/// 记录、不依赖任何根——这正是它能覆盖零痕迹旧根的原因：设备侧在删除目标
/// 不在任何已知商店根之下时按名字比对，命中即拒绝（「无法确认归属时拦住
/// 删除」，fail-closed）。
///
/// 两臂约定名（来源仓库名末段 + 用户模型名）与 [`installed_weight_paths_checked`]
/// 及 `lifecycle::candidate_paths` 同一候选约定，都过 `safe_path_component`
/// 防逃逸；去重后返回，顺序稳定（按记录顺序首现排列）。
///
/// fail-closed：DB 读不了 → Err（与 installed_weight_paths_checked 同口径，
/// 破坏性移除路径不得当未命中放行）。宁可多拒：约定名只由在册卡片占用，
/// 同名非商店目录被拒是可接受的误伤方向（与目录级保护超集同一取舍）。
pub(crate) fn installed_weight_candidate_names_checked() -> Result<Vec<String>, String> {
    let records = db::list_store_models().map_err(|e| format!("读取在册模型失败：{e}"))?;
    let mut out: Vec<String> = Vec::new();
    for rec in &records {
        // 与 installed_weight_paths_checked / lifecycle::candidate_paths 同一
        // 两臂候选约定：来源仓库名末段与用户模型名，都过 safe_path_component。
        if let Some(repo) = rec.source_repo_id.as_deref() {
            if let Some(base) = repo.rsplit('/').next() {
                if let Ok(safe) = config::safe_path_component(base) {
                    if !out.contains(&safe) {
                        out.push(safe);
                    }
                }
            }
        }
        if let Ok(safe) = config::safe_path_component(&rec.model_name) {
            if !out.contains(&safe) {
                out.push(safe);
            }
        }
    }
    Ok(out)
}

/// 官方卡片下载脚本「就地发现」原始模型目录所用的标记文件名全集。
///
/// 证据（脚本正文以商店/仓库下发件为准）：
/// - 正式 Z-Image-Turbo 卡 `download_model.sh`：在扫描根里
///   `find … -maxdepth 4 -type f -name "model_index.json"` 发现原目录，
///   校验完整性（vae 权重 + 两处分片索引的 weight_map 全存在）后
///   `ln -s <原目录> $LMM_MODELS_DIR/Z-Image-Turbo`——原目录名是用户自取的，
///   不必匹配任何约定名（2026-11-05 评审 P2 zhuowp 场景）。
/// - 内置测试商店 ornith15-bf16 / qwen38-27b-nvfp4-mtp 的
///   `download_model.sh`：同一套查找-链接写法，标记文件是
///   `model.safetensors.index.json`。
///
/// 因此在册卡片可能通过软链引用任意名字的原始目录，设备侧删除守卫在
/// 无法确认归属时按标记文件拦截（见
/// [`installed_import_markers_checked`]）。新卡片引入新标记时必须回到这里
/// 登记显式清单（external_models.rs 的拒绝文案也逐一列了标记名，需同步）——
/// 不从脚本正文现解析（对任意卡片脚本做正则提取会漏，
/// 漏掉就是静默盲区，恰是这轮要堵死的那类洞）。
const INSTALL_IMPORT_DISCOVERY_MARKERS: [&str; 2] =
    ["model_index.json", "model.safetensors.index.json"];

/// 设备管理删除守卫的「在册卡片就地导入原目录标记文件」集合（可失败版）。
///
/// 前两臂（[`installed_weight_paths_checked`] 按「已知根 × 约定名」恢复引用、
/// [`installed_weight_candidate_names_checked`] 按名字兜底零痕迹旧根）都依赖
/// 原目录名可推导；zhuowp 场景（2026-11-05 评审 P2）里原目录名是用户自取的，
/// 两臂都失明。本函数换一条腿：官方脚本是**按标记文件**发现原目录的，删除
/// 目标（含一级子目录）出现标记文件即说明它可能是某张在册卡片的就地导入
/// 原目录——归属无法确认，设备侧按 fail-closed 拦截。
///
/// 空库返回空集合（没有任何在册卡片就没有任何未跟踪引用的可能，该臂关闭）；
/// 有在册卡片时返回 [`INSTALL_IMPORT_DISCOVERY_MARKERS`] 全集。
///
/// 误拦面（如实披露）：装有任意在册卡片期间，用户自备的 diffusers / 分片
/// safetensors 目录（叶子或一级子目录含标记文件）同样会被拦——这类目录与
/// 在册卡片的原目录无法区分。解除方式：卸载对应卡片后本臂自动关闭，或按
/// 守卫提示手工删除注册表记录后径直处理目录。
///
/// fail-closed：DB 读不了 → Err（与 installed_weight_paths_checked 同口径，
/// 破坏性移除路径不得当未命中放行）。
pub(crate) fn installed_import_markers_checked() -> Result<Vec<String>, String> {
    let records = db::list_store_models().map_err(|e| format!("读取在册模型失败：{e}"))?;
    if records.is_empty() {
        return Ok(Vec::new());
    }
    Ok(INSTALL_IMPORT_DISCOVERY_MARKERS
        .iter()
        .map(|m| (*m).to_string())
        .collect())
}

/// 设备管理删除守卫的「在册卡片登记的权重文件名」集合（可失败版）。
///
/// 第四条恢复腿（2026-11-06 评审 P2 zhuowp 场景 III，review 5797725367）：
/// 路径臂（约定名 × 已知根）、名字臂（约定名叶/一级）、标记臂（就地导入标记）
/// 共同的盲区是——权重目录名被用户改掉、目录内又没有标记文件，但记录里仍存有
/// 权重文件名（`source_filename`）的在册卡片：三条腿都对它失明，删除其权重
/// 目录的祖先时守卫会放行并连带毁掉它。目录名可以改，权重文件名是卡片运行
/// 的必需组件、不会跟着改——把它作为实际引用数据并入恢复：集合非空时，设备
/// 侧删除目标的子树内出现同名文件即「无法确认不是在册卡片的权重」，按
/// fail-closed 拦截（与标记臂同一走查半径=删除物理范围、同一误拦方向）。
///
/// 集合按记录逐条取 `source_filename`（None/空串跳过，`safe_path_component`
/// 防逃逸——存量记录可能带路径分量），去重后返回。记录连文件名都没存的卡片
/// 在仓库数据里已无任何可恢复的引用痕迹，超出守卫可知边界（如实披露，不虚构
/// 保护面）。空库返回空集合（该臂关闭）。
///
/// fail-closed：DB 读不了 → Err（与前两臂同口径，破坏性移除路径不得当未命中
/// 放行）。
pub(crate) fn installed_weight_filenames_checked() -> Result<Vec<String>, String> {
    let records = db::list_store_models().map_err(|e| format!("读取在册模型失败：{e}"))?;
    let mut out: Vec<String> = Vec::new();
    for rec in &records {
        let Some(filename) = rec.source_filename.as_deref() else {
            continue;
        };
        let trimmed = filename.trim();
        if trimmed.is_empty() {
            continue;
        }
        if let Ok(safe) = config::safe_path_component(trimmed) {
            if !out.contains(&safe) {
                out.push(safe);
            }
        }
    }
    Ok(out)
}

/// 卡片脚本自己落盘的 `.env`（官方卡片共用的 `MODEL_PATH=` 约定）恢复卡片
/// 权重的**权威落点**。证据（脚本正文以商店/仓库下发件为准）：
/// - ornith15-bf16 / qwen38-27b-nvfp4-mtp 的 `download_model.sh`：
///   `sed -i -e "s#^MODEL_PATH=.*#MODEL_PATH=$MODELS_DIR#" .env`——下载完成即
///   把真实权重目录写进 .env；VENDOR_DIR 是 `cards/<id>/vendor`（宿主 prepare
///   约定）或脚本自带的旧约定 `~/.local/share/local-model-manager/vendor/<id>`。
///
/// 这是第五条恢复腿（2026-11-08 评审 P2-3 处方一「覆盖这类依赖」）：前四腿的
/// 恢复数据全部来自商店库与约定推导，对「目录被改名/落在任何已知根之外的
/// 零痕迹旧根」失明；.env 里的 MODEL_PATH 是卡片运行时自己记下的权威路径，
/// 恰好覆盖这两类盲区（改名目录、任意深度祖先删除经路径相交两方向命中）。
/// 五个落点逐一探测（2026-11-09 评审 P2 补旧写入约定，review 5820385522）：
/// 现行宿主两处（`cards/<id>/.env`、`cards/<id>/vendor/.env`）+ 前代 LMM
/// 独立应用的三处——LMM 的数据根是 `~/.local/share/local-model-manager/`
/// （config.rs「不沿用 LMM 的老路径」一节记载，设备上原样保留），其宿主
/// prepare 的卡片目录为 `<老根>/cards/<id>`，`.env` 落在卡片根与
/// `cards/<id>/vendor/`，脚本自带的 vendor 老约定也在同一老根下——老商店
/// 时期装的卡片，其 .env 仍记着现在仍在盘上的权重权威路径。找不到/无
/// MODEL_PATH/路径非绝对按该卡片无数据处理；exists() 过滤（残留 .env 指向
/// 已删除目录时不产生假引用）后 canonicalize 解析软链（与
/// installed_weight_paths_checked 同一把尺）。
///
/// 解析失败的方向是「少恢复」（该卡片退回前四腿的保护面），不是「错恢复」：
/// .env 是卡片脚本可写文件，不把它当信任边界，只当引用恢复的数据源。
fn card_env_model_paths(records: &[db::StoreModelRecord]) -> Vec<std::path::PathBuf> {
    let home = crate::platform::paths::user_home_dir();
    let lmm_root = home.join(".local/share/local-model-manager");
    let mut out: Vec<std::path::PathBuf> = Vec::new();
    for rec in records {
        let Ok(safe_id) = config::safe_path_component(&rec.card_id) else {
            continue;
        };
        let candidates = [
            config::data_dir().join("cards").join(&safe_id).join(".env"),
            config::data_dir()
                .join("cards")
                .join(&safe_id)
                .join("vendor")
                .join(".env"),
            lmm_root.join("cards").join(&safe_id).join(".env"),
            lmm_root
                .join("cards")
                .join(&safe_id)
                .join("vendor")
                .join(".env"),
            lmm_root.join("vendor").join(&safe_id).join(".env"),
        ];
        for env_path in candidates {
            let Ok(text) = std::fs::read_to_string(&env_path) else {
                continue;
            };
            let Some(raw) = parse_env_model_path(&text) else {
                continue;
            };
            let candidate = std::path::PathBuf::from(&raw);
            if !candidate.is_absolute() {
                continue;
            }
            if !candidate.exists() {
                continue;
            }
            let resolved = candidate.canonicalize().unwrap_or(candidate);
            if !out.contains(&resolved) {
                out.push(resolved);
            }
        }
    }
    out
}

/// 解析 `.env` 正文里的 `MODEL_PATH=`（支持 `export ` 前缀与成对引号；多次
/// 赋值取最后一次——与卡片脚本 `sed -i` 覆盖写语义一致）。空值/缺键返回 None。
fn parse_env_model_path(text: &str) -> Option<String> {
    let mut found: Option<String> = None;
    for line in text.lines() {
        let trimmed = line.trim();
        let trimmed = trimmed.strip_prefix("export ").unwrap_or(trimmed).trim();
        if let Some(value) = trimmed.strip_prefix("MODEL_PATH=") {
            let value = value
                .trim()
                .trim_matches('"')
                .trim_matches('\'')
                .trim()
                .to_string();
            if !value.is_empty() {
                found = Some(value);
            }
        }
    }
    found
}

/// 设备管理删除守卫的「归属无法确认的在册卡片」探测（可失败版）。
///
/// 处方二的收窄版（2026-11-08 评审 P2-3：「在归属无法确认时拒绝物理删除」）：
/// 五条恢复腿（引用路径/约定名/导入标记/权重文件名/.env MODEL_PATH）对某张
/// 卡片**全部无数据**（无 source_filename、仓库末段与模型名都过不了
/// `safe_path_component`、卡片 .env 也没有可用的 MODEL_PATH）时，它的权重
/// 可以落在任何位置而守卫无从确认任何删除目标的归属——这类卡片在册期间，
/// 物理删除按整体拒绝处理（解除登记不受影响）；卸载对应卡片后本臂自动放行。
/// 真实触发面：非 ASCII/含路径分量的 model_name 且无来源仓库与文件名的卡片
/// （`safe_path_component` 只放行 `[A-Za-z0-9._-]`，中文自取名卡片正是这个
/// 形状）。仍有任一腿可用（哪怕只是约定名）的卡片不触发本臂——它们由对应
/// 腿按各自的半径保护，本臂不放大拒绝面（与名字臂「宁可漏」的边界处方一致）。
///
/// fail-closed：DB 读不了 → Err（与前四臂同口径，破坏性移除路径不得当未命中
/// 放行）。
pub(crate) fn installed_ownership_unconfirmable_checked() -> Result<bool, String> {
    let records = db::list_store_models().map_err(|e| format!("读取在册模型失败：{e}"))?;
    let filename_usable = |rec: &db::StoreModelRecord| -> bool {
        let Some(filename) = rec.source_filename.as_deref() else {
            return false;
        };
        let trimmed = filename.trim();
        !trimmed.is_empty() && config::safe_path_component(trimmed).is_ok()
    };
    let names_usable = |rec: &db::StoreModelRecord| -> bool {
        let repo_usable = rec
            .source_repo_id
            .as_deref()
            .and_then(|repo| repo.rsplit('/').next())
            .map(|base| config::safe_path_component(base).is_ok())
            .unwrap_or(false);
        repo_usable || config::safe_path_component(&rec.model_name).is_ok()
    };
    for rec in &records {
        if filename_usable(rec) || names_usable(rec) {
            continue;
        }
        if !card_env_model_paths(std::slice::from_ref(rec)).is_empty() {
            continue;
        }
        return Ok(true);
    }
    Ok(false)
}

pub fn storage_settings() -> Result<Value, String> {
    let settings = config::read_settings()?;
    let models_dir = settings.models_path();
    let scan_dirs = import_scan::roots(
        &settings.import_scan_dirs,
        &models_dir,
        &settings.staging_path(),
    );
    Ok(json!({
        "models_dir": models_dir.to_string_lossy(),
        "import_scan_dirs": settings.import_scan_dirs,
        "scan_dirs": scan_dirs
            .iter()
            .map(|p| p.to_string_lossy().to_string())
            .collect::<Vec<_>>(),
        "free_bytes": disk::disk_free_bytes(&models_dir).ok(),
        "startup_update_check_enabled": settings.startup_update_check_enabled,
    }))
}

/// 改权重目录 / 改补充扫描目录。传 `None` 的那项不动。
///
/// **不搬运已下载的权重**：几十上百 GB 的移动可能跨盘、可能中途断电，那是一件
/// 需要独立进度和失败处理的事，不能藏在一次"保存设置"里悄悄发生。改完之后老
/// 目录里的模型在库里仍然记着，但卡片脚本会按新目录找——这一点得在界面上说
/// 清楚，否则用户会以为模型丢了。
///
/// 好在改目录不再是单向的坑：老目录会被 [`import_scan`] 扫到，下载时脚本能把
/// 它认回来复用。
pub fn set_storage_settings(
    models_dir: Option<String>,
    import_scan_dirs: Option<Vec<String>>,
    startup_update_check_enabled: Option<bool>,
) -> Result<Value, String> {
    // Validate all paths before filesystem side effects. Directory preparation
    // stays outside the settings lock; the locked transaction rereads the current
    // roots before it records history and patches the requested fields.
    let models_dir = models_dir
        .map(|dir| {
            let dir = dir.trim().to_string();
            if dir.is_empty() {
                return Err("权重目录不能为空".to_string());
            }
            if !std::path::Path::new(&dir).is_absolute() {
                return Err(format!("权重目录要填绝对路径，收到的是「{dir}」"));
            }
            Ok(dir)
        })
        .transpose()?;
    let import_scan_dirs = import_scan_dirs
        .map(|dirs| {
            let cleaned: Vec<String> = dirs
                .into_iter()
                .map(|dir| dir.trim().to_string())
                .filter(|dir| !dir.is_empty())
                .collect();
            for dir in &cleaned {
                if !std::path::Path::new(dir).is_absolute() {
                    return Err(format!("导入扫描目录要填绝对路径，收到的是「{dir}」"));
                }
            }
            Ok(cleaned)
        })
        .transpose()?;
    if let Some(dir) = &models_dir {
        // 建不出来就不写进配置；0o700：权重清单/日志不该按 umask 落成同机可读。
        {
            use std::os::unix::fs::DirBuilderExt;
            std::fs::DirBuilder::new()
                .mode(0o700)
                .recursive(true)
                .create(dir)
        }
        .map_err(|e| format!("无法创建权重目录「{dir}」：{e}"))?;
    }
    config::update_settings(|settings| {
        if let Some(dir) = models_dir {
            // Changing the active root never moves weights: retain the previous
            // root for the device removal boundary, with main's existing cap.
            if dir != settings.models_dir && !settings.models_dir.trim().is_empty() {
                let old = settings.models_dir.clone();
                settings
                    .models_dir_history
                    .retain(|d| d != &old && d != &dir);
                settings.models_dir_history.push(old);
                let keep = settings
                    .models_dir_history
                    .len()
                    .saturating_sub(config::MAX_MODELS_DIR_HISTORY);
                settings.models_dir_history.drain(..keep);
            }
            settings.models_dir = dir;
        }
        if let Some(cleaned) = import_scan_dirs {
            // Removed scan roots can still contain in-place imported weights.
            let mut removed: Vec<String> = settings
                .import_scan_dirs
                .iter()
                .map(|d| d.trim().to_string())
                .filter(|d| !d.is_empty() && !cleaned.contains(d))
                .collect();
            removed.sort();
            removed.dedup();
            for old in removed {
                settings.import_scan_dirs_history.retain(|d| d != &old);
                settings.import_scan_dirs_history.push(old);
            }
            settings
                .import_scan_dirs_history
                .retain(|d| !cleaned.contains(d));
            let keep = settings
                .import_scan_dirs_history
                .len()
                .saturating_sub(config::MAX_IMPORT_SCAN_DIRS_HISTORY);
            settings.import_scan_dirs_history.drain(..keep);
            settings.import_scan_dirs = cleaned;
        }
        if let Some(enabled) = startup_update_check_enabled {
            settings.startup_update_check_enabled = enabled;
        }
        Ok(())
    })?;
    storage_settings()
}

// ---------------------------------------------------------------- 强制移除

/// 卸载失败留下的死结的唯一出口：**只删库记录，一个字节都不删**。
///
/// 为什么还要一个 `confirm`：生命周期调用很容易被写成「把按钮上的 action 字符串
/// 直接拼进参数」，强制移除必须不能被这种通用循环顺手命中——它不可逆（记录和这张
/// 卡的 API Key 一起没），而且删完之后磁盘上的几十 GB 就再也没有界面能管到了。
pub fn force_remove(
    service: &Arc<ModelStoreService>,
    card_id: String,
    confirm: bool,
) -> Result<Value, String> {
    if service.remote {
        return super::background::call(
            "force_remove",
            json!({"card_id":card_id,"confirm":confirm}),
        );
    }

    if !confirm {
        return Err(
            "强制移除需要显式确认。这个操作只从模型库里删掉记录和该模型的 API Key，\
             **不会删除磁盘上的任何文件**，并且不可撤销。只有在正常卸载已经反复失败\
             （例如脚本报「拒绝删除：目标不像模型目录」）时才用它。"
                .to_string(),
        );
    }
    // 还是要过卡片互斥：真有一个卸载正在删文件时把记录抽走，脚本删到一半就会
    // 对着一个不存在的记录继续跑。
    let _operation_lock = card_operation_lock(&card_id)?;
    let _card_guard = super::runtime_guard::StartupGuard::for_card(&card_id)?;
    service.active.try_begin(&card_id, OP_FORCE_REMOVE, None)?;
    // P2-8（r24）：手工 try_begin/finish 对在 panic 时会跳过 finish——
    // 卡片从此被 ActiveCardOps 永久锁死（此后一律拒绝）。与 spawn_op 的
    // OpCleanup 同理，用 guard 保证 finish 在任何退出路径都执行。
    struct FinishGuard {
        owner: Arc<ModelStoreService>,
        card: String,
    }
    impl Drop for FinishGuard {
        fn drop(&mut self) {
            self.owner.active.finish(&self.card);
        }
    }
    let _finish = FinishGuard {
        owner: Arc::clone(service),
        card: card_id.clone(),
    };
    ModelManager::new(None, None)
        .force_remove_record(&card_id)
        .map_err(|e| e.to_string())
        .and_then(|r| serde_json::to_value(r).map_err(|e| e.to_string()))
}

#[cfg(test)]
mod tests {
    mod recovery;
    use super::*;

    #[test]
    fn confirmed_cms_runtime_survives_proxy_and_ordinary_snapshot_polls() {
        use crate::platform::prefs::{SavedModel, UserPrefs};
        use std::sync::atomic::{AtomicUsize, Ordering};
        let _home = config::TempHome::new();
        let rows: Vec<SavedModel> = serde_json::from_value(json!([
            {"id":"automatic", "name":"Automatic", "preset":"openai_compatible",
            "provider_kind":"custom", "model":"Real", "base_url":"http://127.0.0.1:8000/v1",
            "model_store_card_id":"linked", "store_card_id":"linked"},
            {"id":"manual", "name":"Manual", "preset":"openai_compatible",
            "provider_kind":"custom", "model":"Real", "base_url":"http://127.0.0.1:8000/v1",
            "model_store_card_id":"linked", "store_card_id":"linked", "context_window_tokens":8192,
            "max_output_tokens":2048, "reasoning_effort":"high", "api_key":"preserve-test-value"}
        ]))
        .unwrap();
        UserPrefs::update_transaction(|prefs| {
            prefs.advanced.saved_models = rows.clone();
            Ok(())
        })
        .unwrap();
        // Persistence canonicalizes credentials and inserts the built-in model.
        // Compare the complete saved baseline after that normalization.
        let persisted_rows = UserPrefs::load().advanced.saved_models;
        let manual = persisted_rows
            .iter()
            .find(|row| row.id == "manual")
            .unwrap();
        assert_eq!(manual.context_window_tokens, Some(8192));
        assert_eq!(manual.max_output_tokens, Some(2048));
        assert_eq!(manual.reasoning_effort.as_deref(), Some("high"));
        let host = ModelStoreService::new();
        let desktop = ModelStoreService::with_desktop_host(|| Ok(()));
        let observed = Arc::new(AtomicUsize::new(0));
        let capture = observed.clone();
        desktop.set_models_changed_handler(Arc::new(move |ids| {
            assert_eq!(ids, ["automatic", "manual"]);
            capture.fetch_add(1, Ordering::SeqCst);
        }));
        let payload = |window, revision| {
            json!({
                "linked_runtime_scope":super::super::linked_models::runtime_scope(),
                "models":[{"card_id":"linked", "status":"deployed", "running":true,
                    "endpoint_url":"http://127.0.0.1:8000/v1", "endpoint_model":"Real",
                    "cmsd":{"managed":true,"status":"running","healthy":true,
                        "modelId":"Real","maxModelLen":window,"deploymentRevision":revision}}]
            })
        };
        let deliver = |value: &Value| {
            // Same host observation, JSON response and desktop observation used by library.
            host.observe_linked_runtime(value).unwrap();
            let wire: Value = serde_json::from_slice(&serde_json::to_vec(value).unwrap()).unwrap();
            desktop.observe_linked_runtime(&wire).unwrap();
        };
        deliver(&payload(131072_u64, "rev1"));
        assert_eq!(observed.load(Ordering::SeqCst), 1);
        deliver(&payload(32768, "rev2"));
        assert_eq!(observed.load(Ordering::SeqCst), 2);
        let expected = desktop.linked_revision.lock().clone();
        for _ in 0..3 {
            deliver(&payload(32768, "rev2"));
            desktop.notify_linked_snapshot().unwrap();
        }
        assert_eq!(desktop.linked_revision.lock().clone(), expected);
        assert_eq!(observed.load(Ordering::SeqCst), 2);
        // Unknown/failed/transitional facts and a foreign manager scope do not
        // replace the last confirmed fact or retrigger the model revision.
        for field in ["healthy", "modelId"] {
            let mut unknown = payload(16384, "rev3");
            unknown["models"][0]["cmsd"][field] = Value::Null;
            deliver(&unknown);
        }
        let mut failed = payload(16384, "rev3");
        failed["link_sync_error"] = json!("unavailable");
        deliver(&failed);
        let mut foreign = payload(16384, "rev3");
        foreign["linked_runtime_scope"] = json!("another-profile-manager");
        deliver(&foreign);
        assert_eq!(desktop.linked_revision.lock().clone(), expected);
        assert_eq!(observed.load(Ordering::SeqCst), 2);
        // Deployment and window facts are merged independently. A new positive
        // deployment invalidates once even while its window is unknown.
        let mut deployment_only = payload(32768, "rev4");
        deployment_only["models"][0]["cmsd"]["maxModelLen"] = Value::Null;
        deliver(&deployment_only);
        assert_eq!(observed.load(Ordering::SeqCst), 3);
        assert_eq!(
            desktop.observe_linked_runtime(&deployment_only).unwrap(),
            ["linked"]
        );
        assert_eq!(observed.load(Ordering::SeqCst), 3);
        // Even the same numeric window needs a fresh proof for this deployment:
        // the runtime built while it was unknown may have only a fallback.
        deliver(&payload(32768, "rev4"));
        assert_eq!(observed.load(Ordering::SeqCst), 4);
        let mut partial = payload(32768, "rev4");
        partial["models"][0]["cmsd"]["deploymentRevision"] = Value::Null;
        deliver(&partial);
        deliver(&deployment_only);
        deliver(&payload(32768, "rev4"));
        assert_eq!(observed.load(Ordering::SeqCst), 4);
        let mut stopped = payload(32768, "rev5");
        stopped["models"][0]["running"] = json!(false);
        stopped["models"][0]["cmsd"] = Value::Null;
        let pending = desktop.observe_linked_runtime(&stopped).unwrap();
        assert_eq!(pending, ["linked"]);
        assert_eq!(observed.load(Ordering::SeqCst), 4);
        assert_eq!(UserPrefs::load().advanced.saved_models, persisted_rows);
    }

    #[test]
    fn desktop_uses_one_background_executor_before_component_installation() {
        let called = std::cell::Cell::new(false);
        let service = ModelStoreService::with_desktop_host(|| {
            called.set(true);
            Ok(())
        });
        assert!(called.get());
        assert!(service.is_remote());
        let unavailable = ModelStoreService::with_desktop_host(|| Err("store_offline".into()));
        assert!(
            unavailable.is_remote(),
            "host failure must not create another queue"
        );
    }

    #[test]
    fn dropping_an_unstarted_worker_releases_its_card_and_finishes_the_log() {
        let service = Arc::new(ModelStoreService::new());
        service
            .active
            .try_begin("card", OP_ENABLE, Some("run"))
            .unwrap();
        let log = Arc::new(RunLog::new_memory_only());
        let control = service.registry.register("run");
        let guard = OpCleanup {
            _card_guard: None,
            registry_owner: Arc::clone(&service),
            card: "card".into(),
            run_id: "run".into(),
            log: Arc::clone(&log),
            control,
            outcome: Some(Err("worker unavailable".into())),
            operation_lock: None,
        };
        // Builder::spawn drops its captured closure when thread creation fails.
        // Simulate that path without exhausting the host's thread quota.
        let worker = move || {
            let _guard = guard;
        };
        drop(worker);
        assert!(service.active.run_for_card("card").is_none());
        assert!(log.finished_at.lock().is_some());
        assert!(log.outcome.lock().as_ref().unwrap().is_err());
    }

    #[test]
    fn docker_layer_refreshes_are_recognized_without_hiding_real_messages() {
        assert!(is_docker_layer_progress_line(
            "[安装引擎] ae5a106b6076: Pulling fs layer"
        ));
        assert!(is_docker_layer_progress_line(
            "7014a14111a1: Downloading [====>    ] 1.967GB/4.07GB"
        ));
        assert!(is_docker_layer_progress_line("abcdef123456: Pull complete"));

        // 这两类信息能解释下载结果，绝不能为了节省日志空间一并吞掉。
        assert!(!is_docker_layer_progress_line(
            "Status: Downloaded newer image for lmsysorg/sglang:v0.5.16-cu130"
        ));
        assert!(!is_docker_layer_progress_line(
            "脚本执行失败：镜像源返回 429"
        ));
        assert!(!is_docker_layer_progress_line(
            "not-a-layer: Downloading [====>] 1GB/4GB"
        ));
    }

    #[tokio::test]
    async fn enable_synchronizes_linked_route_and_notifies_after_commit_without_a_view() {
        use crate::platform::prefs::{SavedModel, UserPrefs};
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        db::add_store_model(db::AddStoreModelParams {
            card_id: "sync-card",
            model_name: "store-model",
            card_name: "Local",
            org: None,
            description: None,
            engine_name: "native",
            engine_version: None,
            size_bytes: 1,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &Default::default(),
            status: "deployed",
            profiles: &[],
            deploy_topology: super::super::store_client::DeployTopology::Single,
            card_source: "store",
        })
        .unwrap();
        db::set_endpoint_info(
            "sync-card",
            Some("http://127.0.0.1:8000/v1"),
            Some("old-model"),
        )
        .unwrap();
        let script = "#!/bin/sh\necho LMM_ENDPOINT_URL=http://127.0.0.1:8001/v1/chat/completions\necho LMM_ENDPOINT_MODEL=new-model\n";
        let scripts = std::collections::BTreeMap::from([(
            "enable".into(),
            db::ScriptBlob {
                content: script.into(),
                sha256: script_runner::sha256_hex(script),
            },
        )]);
        db::set_scripts_content("sync-card", &scripts, &Default::default()).unwrap();
        let row: SavedModel =
            serde_json::from_value(json!({"id":"stable-id", "name":"Custom label",
            "preset":"openai_compatible", "provider_kind":"custom", "model":"old-model",
            "base_url":"http://127.0.0.1:8000/v1", "context_window_tokens":131072}))
            .unwrap();
        let mut responses = row.clone();
        responses.id = "manual-responses".into();
        responses.preset = crate::platform::prefs::ModelPreset::OpenaiResponses;
        let mut messages = row.clone();
        messages.id = "manual-messages".into();
        messages.vendor = Some("anthropic".into());
        let mut full_endpoint = row.clone();
        full_endpoint.id = "manual-full-endpoint".into();
        full_endpoint.endpoint_mode = Some("full_chat_completions".into());
        let manual_rows = vec![responses, messages, full_endpoint];
        UserPrefs::update_transaction(|prefs| {
            prefs.advanced.saved_models.push(row);
            prefs.advanced.saved_models.extend(manual_rows.clone());
            prefs.advanced.active_model_id = Some("stable-id".into());
            Ok(())
        })
        .unwrap();
        let observed = Arc::new(Mutex::new(Vec::new()));
        let observation = Arc::clone(&observed);
        let service = Arc::new(ModelStoreService::new());
        service.set_models_changed_handler(Arc::new(move |ids| {
            let prefs = UserPrefs::load();
            observation.lock().push((
                ids.to_vec(),
                prefs.model_by_id("stable-id").unwrap().model.clone(),
            ));
        }));
        let started = start_lifecycle(&service, "sync-card".into(), "enable".into(), false, false)
            .await
            .unwrap();
        let log = service.run(started["run_id"].as_str().unwrap()).unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        while log.outcome.lock().is_none() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(log.outcome.lock().as_ref().is_some_and(Result::is_ok));
        let observations = observed.lock();
        assert!(
            observations
                .iter()
                .any(|(ids, model)| ids == &["stable-id"] && model == "new-model")
        );
        assert!(observations.iter().all(|(ids, _)| ids == &["stable-id"]));
        let reloaded = UserPrefs::load();
        let row = reloaded.model_by_id("stable-id").unwrap();
        assert_eq!(row.base_url, "http://127.0.0.1:8001/v1");
        assert_eq!(row.name, "Custom label");
        assert_eq!(row.context_window_tokens, Some(131072));
        assert_eq!(
            reloaded.advanced.active_model_id.as_deref(),
            Some("stable-id")
        );
        // Pre-enable binding and the post-enable sync must both leave manual
        // protocols alone. Without provenance these rows also remain ineligible
        // for the uninstall UI and its conditional store-link deletion command.
        for expected in &manual_rows {
            assert_eq!(reloaded.model_by_id(&expected.id), Some(expected));
            assert!(expected.model_store_card_id.is_none());
        }
        assert_eq!(
            super::super::linked_models::current_linked_ids("sync-card"),
            ["stable-id"]
        );
        drop(observations);
        // A second enable keeps the exact same route. Its engine may now serve
        // a different context profile, so the next turn still needs a fresh probe.
        observed.lock().clear();
        // A separate desktop instance observes a background enable even though
        // no preference, endpoint or model ID changes during this second run.
        let desktop = ModelStoreService::with_desktop_host(|| Ok(()));
        desktop.notify_linked_snapshot().unwrap();
        let invalidated = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let counter = Arc::clone(&invalidated);
        desktop.set_models_changed_handler(Arc::new(move |ids| {
            assert_eq!(ids, ["stable-id"]);
            counter.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        }));
        let restarted =
            start_lifecycle(&service, "sync-card".into(), "enable".into(), false, false)
                .await
                .unwrap();
        let restarted_log = service.run(restarted["run_id"].as_str().unwrap()).unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        while restarted_log.outcome.lock().is_none() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(
            restarted_log
                .outcome
                .lock()
                .as_ref()
                .is_some_and(Result::is_ok)
        );
        assert_eq!(observed.lock().len(), 1);
        assert_eq!(observed.lock()[0].0, ["stable-id"]);
        desktop.notify_linked_snapshot().unwrap();
        assert_eq!(invalidated.load(std::sync::atomic::Ordering::SeqCst), 1);
        desktop.notify_linked_snapshot().unwrap();
        assert_eq!(invalidated.load(std::sync::atomic::Ordering::SeqCst), 1);
        assert_eq!(
            UserPrefs::load().advanced.saved_models,
            reloaded.advanced.saved_models
        );
        // Routine library synchronization must not repeatedly invalidate engines.
        observed.lock().clear();
        service.sync_linked_models(None).unwrap();
        assert!(observed.lock().is_empty());
        // Explicit recovery notifies only after the changed settings are durable.
        follow_deployment_context(Arc::clone(&service), "sync-card".into(), "stable-id".into())
            .await
            .unwrap();
        assert_eq!(observed.lock().len(), 1);
        assert!(
            UserPrefs::load()
                .model_by_id("stable-id")
                .unwrap()
                .context_window_tokens
                .is_none()
        );
    }

    #[test]
    fn cancellation_after_a_successful_commit_is_reported_as_success() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let service = Arc::new(ModelStoreService::new());
        let started = spawn_op(&service, "committed-card", OP_DEPLOY, |_, control, _| {
            control
                .cancel
                .as_ref()
                .unwrap()
                .store(true, std::sync::atomic::Ordering::SeqCst);
            Ok(json!({"committed":true}))
        })
        .unwrap();
        let log = service.run(started["run_id"].as_str().unwrap()).unwrap();
        let deadline = Instant::now() + Duration::from_secs(3);
        while log.outcome.lock().is_none() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(log.outcome.lock().as_ref().is_some_and(Result::is_ok));
        assert!(!*log.cancelled.lock());
    }

    /// 「现在还有没有提示在等人答」只有后端知道，而这件事必须由后端说了算。
    ///
    /// 界面是从日志行里解析提示的，而日志可以回放（切走再回来会从头读一遍）。
    /// 答过的提示被回放时再弹一次的后果不是多点一下：用户填的答案会写进一个没人
    /// 来取的应答槽，等脚本下一次真的提问时被这份陈答案直接顶掉——双机部署里
    /// 那就是拿着上一轮的 IP 去连从机。
    #[test]
    fn a_prompt_stops_awaiting_once_anything_else_comes_out() {
        let log = RunLog::new_memory_only();
        assert!(!*log.awaiting_prompt.lock(), "还没问就不该在等");

        log.push("LMM_PROMPT=text|请输入从机 IP");
        assert!(*log.awaiting_prompt.lock(), "提示行一出现就是在等作答");

        // 脚本再打出任何一行，就说明它已经读到答案往下走了
        log.push("正在连接从机 …");
        assert!(!*log.awaiting_prompt.lock());

        // 表单式提问同样算
        log.push("LMM_PROMPT_FORM={\"fields\":[{\"id\":\"ip\"}]}");
        assert!(*log.awaiting_prompt.lock());
    }

    /// 答完立刻落标志，不等脚本打下一行。
    ///
    /// 用户完全可能答完就切走，而脚本这时候还在闷头干活一声不吭；那一瞬间切回来
    /// 的话，回放会把这条已经答过的提示又弹一次。
    #[test]
    fn answering_clears_awaiting_before_the_script_says_anything() {
        let log = RunLog::new_memory_only();
        log.push("LMM_PROMPT=password|输入本机密码");
        log.answered();
        assert!(!*log.awaiting_prompt.lock());
    }

    /// MegaCube 融合：cmsd 实况挂进每行的 `cmsd` 字段；不认识的卡不动。
    #[test]
    fn attach_cmsd_states_maps_by_card_id() {
        let mut payload = serde_json::json!([
            {"card_id": "ornith15-35b-a3b-nvfp4", "running": false},
            {"card_id": "qwen38-27b-sglang0519", "running": false},
        ]);
        let mut states = std::collections::BTreeMap::new();
        states.insert(
            "ornith15-35b-a3b-nvfp4".to_string(),
            crate::features::megacube::delegate::CmsdModelState {
                managed: true,
                can_adopt: false,
                can_reserve_uninstall: true,
                model_id: None,
                status: "running".to_string(),
                healthy: Some(true),
                restart_count: 2,
                degraded: false,
                degraded_reason: None,
                port: Some(18080),
                measured_footprint_gib: Some(33.0),
                max_model_len: Some(262144),
                deployment_revision: Some("recipe-current".into()),
                version_id: Some("v1".into()),
            },
        );
        attach_cmsd_states(&mut payload, &states);
        let items = payload.as_array().unwrap();
        assert_eq!(items[0]["cmsd"]["status"], "running");
        assert_eq!(items[0]["cmsd"]["restartCount"], 2);
        assert_eq!(items[0]["cmsd"]["maxModelLen"], 262144);
        assert_eq!(items[0]["cmsd"]["deploymentRevision"], "recipe-current");
        assert_eq!(items[0]["cmsd"]["versionId"], "v1");
        assert_eq!(items[0]["cmsd"]["port"], 18080);
        assert!(items[1].get("cmsd").is_none(), "cmsd 不认识的卡不挂字段");
    }

    /// 「失败后保留原生进程」是一次性声明：置位后不随后续行回退，运行结束也保留——
    /// 结束后正是残存进程占内存的时段，「二次确认停用」入口恰恰这时候最不能消失。
    /// 2026-10-20 评审 A5：此前前端只能从日志行里认这行声明，回放一裁剪（末尾
    /// 200 行）标志就丢。结构化字段跟着 run_log 走，行被裁掉声明仍在。
    #[test]
    fn retained_native_declaration_is_sticky() {
        let log = RunLog::new_memory_only();
        assert!(!*log.retained_native.lock());

        // 协议行经 prefix_log_line 原样透传（不加阶段前缀），到达这里就是裸行
        log.push("LMM_RETAINED_NATIVE_PROCESS=1");
        assert!(*log.retained_native.lock(), "声明行一出现就置位");

        // 后续输出不撤销声明
        log.push("引擎进程已保留，等待手动停止");
        assert!(*log.retained_native.lock());

        // 结束也不撤销——任务过期后仍要有常驻的停用入口
        log.finish(Err("启用失败".to_string()), false);
        assert!(*log.retained_native.lock());
    }

    /// 跑完了就没有什么可答的了。留着这个标志，界面接回一条已结束的运行时会弹出
    /// 一个永远送不出去的输入框。
    #[test]
    fn finishing_clears_awaiting() {
        let log = RunLog::new_memory_only();
        log.push("LMM_PROMPT=confirm|继续吗");
        log.finish(Err("脚本非零退出".to_string()), false);
        assert!(!*log.awaiting_prompt.lock());
    }

    /// 行数封顶之后，长轮询的**绝对游标**语义不能变。
    ///
    /// 前端握着的 `from` 是绝对序号。丢掉最老的行时若把序号重算成 0，同一个游标会
    /// 指到别的行上——界面会把一段日志重收一遍，而在有 `LMM_PROMPT=` 的路径上
    /// "重收一遍"就等于把答过的提示再弹一次（那条路的后果见
    /// `a_prompt_stops_awaiting_once_anything_else_comes_out`）。
    #[test]
    fn capping_lines_keeps_absolute_cursors_monotonic() {
        let log = RunLog::new_memory_only();
        for index in 0..(LOG_TAIL_LINES + 10) {
            log.push(&format!("line-{index}"));
        }
        // 总行数照实数，不因为丢了行就缩回去
        let dropped = *log.line_offset.lock();
        assert_eq!(dropped + log.lines.lock().len(), LOG_TAIL_LINES + 10);
        assert_eq!(dropped, 10);
        // 游标落在还留着的区间：从那一行开始给
        let (page, _) = log
            .snapshot(LOG_TAIL_LINES + 8, Default::default())
            .unwrap();
        assert_eq!(page.from, LOG_TAIL_LINES + 8);
        assert_eq!(
            page.lines,
            vec![
                format!("line-{}", LOG_TAIL_LINES + 8),
                format!("line-{}", LOG_TAIL_LINES + 9)
            ]
        );
        // 游标落在已经丢掉的区间：从现存最老的那一行开始给，**不是**从 0
        let (stale, _) = log.snapshot(0, Default::default()).unwrap();
        assert_eq!(stale.from, 10);
        assert_eq!(stale.lines.first().map(String::as_str), Some("line-10"));
        // 游标已经追到头：给空，别让轮询以为有新行
        let (empty, _) = log
            .snapshot(LOG_TAIL_LINES + 10, Default::default())
            .unwrap();
        assert!(empty.lines.is_empty());
    }

    #[test]
    fn prompt_markers_do_not_shadow_each_other() {
        assert!(is_prompt_line("LMM_PROMPT=text|要点什么"));
        assert!(is_prompt_line("  LMM_PROMPT_FORM={}  "));
        assert!(!is_prompt_line("LMM_PROMPTX=1"));
        assert!(!is_prompt_line("正在下载 …"));
    }

    /// 进度行解析：跟前端 `model-store-logic.js` 的正则同语义——行内任意位置
    /// 出现的最后一条 `LMM_PROGRESS_BYTES=<数字>/<数字>`。
    #[test]
    fn progress_marker_parses_from_the_last_matching_line() {
        let lines = vec![
            "开始".to_string(),
            "[下载模型] LMM_PROGRESS_BYTES=10/100".to_string(),
            "LMM_PROGRESS_BYTES=50/100".to_string(),
            "别的输出".to_string(),
        ];
        assert_eq!(parse_progress(&lines), Some((50, 100)));
        assert_eq!(parse_progress(&["没有进度".to_string()]), None);
        assert_eq!(
            parse_progress(&["LMM_PROGRESS_BYTES=x/y".to_string()]),
            None
        );
        assert_eq!(parse_progress(&["LMM_PROGRESS_BYTES=10".to_string()]), None);
        assert_eq!(parse_progress(&[]), None);
    }

    /// 任务快照：结局到状态的映射。取消必须单列一态——「你点了取消」和「它炸了」
    /// 对 cmsd 排障剧本是两件事（一个重试没意义，一个可能值得换个方式再来）。
    #[test]
    fn task_snapshot_maps_outcome_to_status_and_keeps_cancel_separate() {
        let log = RunLog::new_without_disk("card-x", "download");
        log.push("第一行");
        log.push("[下载模型] LMM_PROGRESS_BYTES=1/2");
        let snap = log.to_task_snapshot("run-1", 500);
        assert_eq!(snap.status, "running");
        assert_eq!(snap.card_id, "card-x");
        assert_eq!(snap.kind, "download");
        assert_eq!(snap.progress, Some((1, 2)));
        assert_eq!(
            snap.last_line.as_deref(),
            Some("[下载模型] LMM_PROGRESS_BYTES=1/2")
        );
        assert_eq!(snap.lines.len(), 2, "tail=500 时全部行都带上");
        assert!(snap.error.is_none());
        assert!(!snap.created_at.is_empty());

        log.finish(Err("已取消".to_string()), true);
        let snap = log.to_task_snapshot("run-1", 500);
        assert_eq!(snap.status, "cancelled", "取消必须单列一态，不等同失败");
        assert_eq!(snap.error.as_deref(), Some("已取消"));

        let failed = RunLog::new_without_disk("card-y", "install_local");
        failed.finish(Err("脚本执行失败（退出码 1）".to_string()), false);
        let snap = failed.to_task_snapshot("run-2", 0);
        assert_eq!(snap.status, "failed");
        assert_eq!(snap.kind, "install_local");
        assert!(snap.lines.is_empty(), "tail=0 不带日志行（列表接口）");

        let succeeded = RunLog::new_without_disk("card-z", "download");
        succeeded.finish(Ok(json!({})), false);
        assert_eq!(succeeded.to_task_snapshot("run-3", 0).status, "succeeded");

        // 失败但 cancelled 标记为 false（脚本自己打的「已取消」字样的错误文案）
        // 不能算取消——状态以标记为准，不以文字为准。
        let looks = RunLog::new_without_disk("card-w", "download");
        looks.finish(Err("已取消".to_string()), false);
        assert_eq!(looks.to_task_snapshot("run-4", 0).status, "failed");
    }

    #[test]
    fn abandon_missing_task_fences_late_requests_and_refuses_corrupt_or_busy_state() {
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let _home = config::TempHome::new();
        let service = Arc::new(ModelStoreService::new());
        assert!(
            service
                .abandon_missing_run(
                    "same-boot",
                    "card",
                    "download",
                    chrono::Utc::now().timestamp() as u64
                )
                .unwrap_err()
                .contains("reboot_required")
        );
        assert!(
            service
                .abandon_missing_run("unknown-admission", "card", "download", 0)
                .is_err()
        );
        assert!(service.lookup_run("same-boot").unwrap().is_none());
        service
            .abandon_missing_run("lost-task", "lost-card", "install_local", 1)
            .unwrap();
        let reply = spawn_op_with_id(
            &service,
            "lost-card",
            OP_INSTALL_LOCAL,
            Some("lost-task".into()),
            |_, _, _| panic!("abandoned exact id must never execute"),
        )
        .unwrap();
        assert_eq!(reply["run_id"], "lost-task");
        assert_eq!(
            service.task_snapshot("lost-task", 0).unwrap().status,
            "cancelled"
        );
        let active = service
            .new_run("active", "active-card", "download")
            .unwrap();
        assert!(
            service
                .abandon_missing_run("another", "card", "download", 1)
                .unwrap_err()
                .contains("busy")
        );
        active.finish(Ok(Value::Null), false);
        let mut child = std::process::Command::new("sleep")
            .arg("20")
            .spawn()
            .unwrap();
        let guard = super::super::orphans::RunGuard::register_with_run_id(
            child.id() as i32,
            false,
            "test writer",
            Some("old-writer"),
        );
        let blocked = service.abandon_missing_run("live-writer", "card", "download", 1);
        child.kill().unwrap();
        child.wait().unwrap();
        drop(guard);
        assert!(
            blocked.is_err(),
            "a live orphan must retain the installation exclusion"
        );
        service
            .abandon_missing_run("live-writer", "card", "download", 1)
            .unwrap();
        std::fs::write(config::data_dir().join("runs/corrupt.json"), b"{").unwrap();
        assert!(
            service
                .abandon_missing_run("corrupt", "card", "download", 1)
                .is_err()
        );
        super::super::execution_fence::private_write(
            &config::data_dir().join("running-scripts.json"),
            b"corrupt",
        )
        .unwrap();
        assert!(
            service
                .abandon_missing_run("unknown-process", "card", "download", 1)
                .is_err()
        );
    }

    #[test]
    fn retained_log_tail_keeps_absolute_cursors_across_restart() {
        let _home = config::TempHome::new();
        let service = ModelStoreService::new();
        let log = service.new_run("tail", "card", "download").unwrap();
        log.push("LMM_PROGRESS_BYTES=1/100");
        for index in 0..5000 {
            log.push(&format!("{index}:{}", "x".repeat(1024)));
        }
        let offset = *log.line_offset.lock();
        assert!(offset > 0);
        assert!(log.lines.lock().iter().map(String::len).sum::<usize>() <= LOG_TAIL_BYTES);
        let (page, _) = log.snapshot(0, Default::default()).unwrap();
        assert_eq!(page.from, offset);
        assert_eq!(page.total, 5001);
        assert_eq!(log.to_task_snapshot("tail", 0).progress, Some((1, 100)));
        assert!(page.next > offset);
        log.finish(Ok(Value::Null), false);
        let saved = load_saved_run(&config::data_dir().join("runs/tail.json"))
            .unwrap()
            .unwrap();
        assert_eq!(*saved.line_offset.lock(), offset);
        assert_eq!(
            saved.snapshot(0, Default::default()).unwrap().0.from,
            offset
        );
        // A legacy producer could have persisted a much larger hot tail.
        let path = config::data_dir().join("runs/tail.json");
        let mut legacy: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        legacy["lines"] = json!(vec!["猫".repeat(1024); 2500]);
        legacy["line_offset"] = json!(123);
        super::super::execution_fence::private_write(&path, &serde_json::to_vec(&legacy).unwrap())
            .unwrap();
        let restored = load_saved_run(&path).unwrap().unwrap();
        let page = restored.snapshot(0, Default::default()).unwrap().0;
        assert_eq!(page.total, 2623);
        assert!(page.from > 123);
        assert!(*restored.line_bytes.lock() <= LOG_TAIL_BYTES);
        assert_eq!(
            restored.to_task_snapshot("tail", 0).progress,
            Some((1, 100))
        );
        let again = load_saved_run(&path).unwrap().unwrap();
        assert_eq!(
            again.snapshot(0, Default::default()).unwrap().0.from,
            page.from
        );
    }

    #[test]
    fn corrupt_history_does_not_take_other_tasks_offline_or_allow_id_reuse() {
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let _home = config::TempHome::new();
        let service = ModelStoreService::new();
        service
            .new_run("healthy", "card-a", "download")
            .unwrap()
            .finish(Ok(json!({})), false);
        let root = config::data_dir().join("runs");
        std::fs::write(root.join("corrupt.json"), b"{").unwrap();
        std::fs::create_dir(root.join("directory.json")).unwrap();
        let restored = ModelStoreService::new();
        restored.restore_runs().unwrap();
        assert_eq!(
            restored.task_snapshot("healthy", 0).unwrap().status,
            "succeeded"
        );
        assert!(restored.lookup_run("corrupt").is_err());
        assert!(root.join("corrupt.json").exists());
        assert!(restored.lookup_run("directory").is_err());
    }

    #[test]
    fn persistent_runs_survive_host_restart_without_resubmission() {
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let _home = config::TempHome::new();
        let first = ModelStoreService::new();
        let pending = first
            .new_run("cmsd-test", "card-a", "install_local")
            .unwrap();
        let uninstall = first
            .new_run("uninstall-test", "card-u", "uninstall")
            .unwrap();
        uninstall.persist();
        pending.push("LMM_PROGRESS_BYTES=8/20");
        pending.persist();
        let completed = first.new_run("done-test", "card-b", "download").unwrap();
        completed.finish(Ok(json!({"installed":true})), false);
        drop(first);
        let restored = Arc::new(ModelStoreService::new());
        restored.restore_runs().unwrap();
        let pending = restored.task_snapshot("cmsd-test", 100).unwrap();
        assert_eq!(pending.status, "failed");
        assert!(pending.error.unwrap().contains("interrupted"));
        assert_eq!(pending.progress, Some((8, 20)));
        let uninstall = restored.task_snapshot("uninstall-test", 0).unwrap();
        assert!(uninstall.error.unwrap().contains("未自动重跑卸载"));
        assert_eq!(
            restored.task_snapshot("done-test", 0).unwrap().status,
            "succeeded"
        );
        // Retrying the old transport request resolves the old handle; the job
        // callback is never executed, even though the host process restarted.
        let maintenance = config::data_dir().join("maintenance");
        std::fs::create_dir_all(&maintenance).unwrap();
        // Even an unreadable pending transaction cannot turn a transport retry
        // into another job or hide the handle that was already accepted.
        std::fs::write(maintenance.join("card-a.json"), b"{").unwrap();
        let reply = spawn_op_with_id(
            &restored,
            "card-a",
            OP_INSTALL_LOCAL,
            Some("cmsd-test".into()),
            |_, _, _| panic!("must not rerun"),
        )
        .unwrap();
        assert_eq!(reply["run_id"], "cmsd-test");
        assert!(
            spawn_op_with_id(
                &restored,
                "card-a",
                OP_INSTALL_LOCAL,
                Some("new-request".into()),
                |_, _, _| panic!("must not start a new job during maintenance"),
            )
            .is_err()
        );
        assert!(
            spawn_op_with_id(
                &restored,
                "other-card",
                OP_INSTALL_LOCAL,
                Some("cmsd-test".into()),
                |_, _, _| Ok(Value::Null)
            )
            .is_err()
        );
    }

    /// 列表：新的在前，列表项不带日志行（几百个任务各自拖几百行，列表应答
    /// 会胖得没法看）。
    #[test]
    fn task_snapshots_are_newest_first_and_carry_no_lines() {
        let _lock = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        let _home = config::TempHome::new();
        let svc = ModelStoreService::new();
        let first = svc.new_run("run-a", "card-a", "download").unwrap();
        first.push("一些日志");
        let _second = svc.new_run("run-b", "card-b", "install_local").unwrap();
        let snaps = svc.task_snapshots();
        assert_eq!(snaps.len(), 2);
        assert_eq!(snaps[0].run_id, "run-b");
        assert_eq!(snaps[1].run_id, "run-a");
        assert!(snaps.iter().all(|s| s.lines.is_empty()));
        assert!(snaps.iter().all(|s| !s.created_at.is_empty()));

        // 单任务详情按 run_id 取，带末尾日志；取不到的是 None（过期清扫同语义）
        let detail = svc.task_snapshot("run-a", 500).expect("找得到 run-a");
        assert_eq!(detail.lines, vec!["一些日志".to_string()]);
        assert!(svc.task_snapshot("run-nope", 500).is_none());
    }

    /// `notify_waiters()` **不预存许可**——这是长轮询里顺序不能反的全部原因。
    ///
    /// 把这条单独钉出来，是因为它是那个 bug 的根：读代码的人很容易以为「push 里
    /// 已经通知了，谁来等都能收到」。实际上没有等待者时这次通知直接丢弃，后来
    /// 才登记的等待者拿不到任何东西，只能挂到超时。
    ///
    /// 这条同时是一道护栏：谁要是把 `push` 改成 `notify_one()`（它**会**存一个
    /// 许可，看起来「更安全」），这条会红——而那个改法会让多个并发长轮询里只有
    /// 一个被唤醒，是另一个更难查的错。唤醒的安全性必须来自登记顺序，不是许可。
    #[tokio::test(start_paused = true)]
    async fn notify_waiters_drops_the_wakeup_when_nobody_is_waiting_yet() {
        let log = RunLog::new_memory_only();

        // 此刻没有任何等待者，这一次唤醒被丢弃
        log.push("先推一行");

        // 之后才登记的等待者收不到它 —— 所以「先 snapshot 再 notified()」会丢唤醒
        let late = log.notify.notified();
        assert!(
            tokio::time::timeout(Duration::from_secs(5), late)
                .await
                .is_err(),
            "notify_waiters 竟然预存了许可：那长轮询里的顺序就无所谓了，             这条测试和 run_log 里那段注释都要重写"
        );
    }

    /// 长轮询必须**先把自己登记成等待者、再取快照**。
    ///
    /// 真正的竞态发生在 `run_log` 里相邻两条语句之间（worker 恰好在 snapshot 之后、
    /// 登记之前 push），从测试外部注入不了这个时机，所以这里退一步断源码结构——
    /// 跟 `tauri_cross_build_gate.test.js` 对备料守卫的做法一致。等价重构（改名、
    /// 换等待方式）不会误伤，把两行调回原来的顺序则必红。
    ///
    /// 会红意味着什么：脚本问一句话，用户最坏要等满 `LOG_WAIT`（25 秒）才看见。
    #[test]
    fn the_long_poll_registers_its_waiter_before_taking_the_snapshot() {
        const SERVICE_SOURCE: &str = include_str!("service.rs");
        // 合并修正（r23）：main 侧把实现拆进 run_log_page（pub async fn run_log
        // 只是薄委托），等待/快照逻辑的真身在这里——契约跟着实现走。
        let body = SERVICE_SOURCE
            .split("async fn run_log_page(")
            .nth(1)
            .expect("run_log_page 没了？这条测试要跟着改");
        let registered = body
            .find("notified.as_mut().enable()")
            .expect("run_log_page 里找不到 enable()：等待者没有被提前登记，会丢唤醒");
        let snapshot = body
            .find("log.snapshot(from")
            .expect("run_log_page 里找不到 snapshot 调用");
        assert!(
            registered < snapshot,
            "登记等待者必须在取快照之前。现在反了——worker 在这两步之间 push 的行             会丢唤醒，本次长轮询要挂满 25 秒，而这条路径上跑的是交互提示。"
        );
    }
    #[test]
    fn storage_updates_preserve_the_startup_opt_out_and_failed_saves_do_not_apply_it() {
        let _home = config::TempHome::new();
        let first = set_storage_settings(None, None, Some(false)).unwrap();
        assert_eq!(first["startup_update_check_enabled"], false);
        let second = set_storage_settings(None, Some(vec!["/extra/models".into()]), None).unwrap();
        assert_eq!(second["startup_update_check_enabled"], false);
        assert_eq!(
            second["import_scan_dirs"],
            serde_json::json!(["/extra/models"])
        );
        let temporary = config::data_dir().join("config.json.tmp");
        std::fs::create_dir(&temporary).unwrap();
        assert!(set_storage_settings(None, None, Some(true)).is_err());
        assert!(!config::startup_update_check_enabled().unwrap());
        std::fs::remove_dir(temporary).unwrap();
        assert_eq!(
            set_storage_settings(None, None, Some(true)).unwrap()["startup_update_check_enabled"],
            true
        );
    }

    #[tokio::test]
    async fn direct_startup_command_respects_opt_out_without_consuming_the_attempt() {
        let _home = config::TempHome::new();
        set_storage_settings(None, None, Some(false)).unwrap();
        let service = Arc::new(ModelStoreService::new());
        assert_eq!(
            startup_update_check(Arc::clone(&service)).await.unwrap()["checked"],
            0
        );
        let probe = service
            .startup_updates
            .once(true, || super::super::startup_updates::Report {
                checked: 7,
                ..Default::default()
            })
            .await;
        assert_eq!(probe.checked, 7);
    }
    #[test]
    fn card_operation_lock_is_shared_by_independent_service_instances() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let first = Arc::new(ModelStoreService::new());
        let second = Arc::new(ModelStoreService::new());
        let (release, wait) = std::sync::mpsc::channel();
        let started = spawn_op(&first, "shared-card", OP_DEPLOY, move |_, _, _| {
            wait.recv().unwrap();
            Ok(Value::Null)
        })
        .unwrap();
        assert!(spawn_op(&second, "shared-card", OP_ENABLE, |_, _, _| Ok(Value::Null)).is_err());
        assert!(
            force_remove(&second, "shared-card".into(), true)
                .unwrap_err()
                .contains("其他窗口")
        );
        release.send(()).unwrap();
        let log = first.run(started["run_id"].as_str().unwrap()).unwrap();
        let deadline = Instant::now() + Duration::from_secs(3);
        while log.outcome.lock().is_none() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(log.outcome.lock().as_ref().is_some_and(Result::is_ok));
        assert!(spawn_op(&second, "shared-card", OP_ENABLE, |_, _, _| Ok(Value::Null)).is_ok());
    }

    #[test]
    fn card_operation_lock_refuses_symlinks_and_tightens_existing_permissions() {
        use std::os::unix::fs::{PermissionsExt, symlink};
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        let lock_dir = config::data_dir().join("operation-locks");
        std::fs::create_dir_all(&lock_dir).unwrap();
        let target = lock_dir.join("unrelated");
        std::fs::write(&target, "keep").unwrap();
        std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o644)).unwrap();
        symlink(&target, lock_dir.join("linked.lock")).unwrap();
        assert!(card_operation_lock("linked").is_err());
        assert_eq!(std::fs::read_to_string(&target).unwrap(), "keep");
        assert_eq!(
            std::fs::metadata(&target).unwrap().permissions().mode() & 0o777,
            0o644
        );
        let existing = lock_dir.join("existing.lock");
        std::fs::write(&existing, "").unwrap();
        std::fs::set_permissions(&existing, std::fs::Permissions::from_mode(0o644)).unwrap();
        let _lock = card_operation_lock("existing").unwrap();
        assert_eq!(
            std::fs::metadata(&existing).unwrap().permissions().mode() & 0o777,
            0o600
        );
    }

    #[test]
    fn changing_models_dir_records_the_old_dir_in_history() {
        let ctx = super::config::TempHome::new();
        let a = ctx.path().join("weights-a");
        let b = ctx.path().join("weights-b");
        let path_of = |p: &std::path::Path| p.to_string_lossy().to_string();

        // 默认（回填）→ A：旧值（数据根下的 models/）记入历史——它本就在
        // 保护根里，重复无害；关键是 A 不出现在历史。
        set_storage_settings(Some(path_of(&a)), None, None).expect("set first models_dir");
        let settings = super::config::load_settings();
        assert_eq!(settings.models_dir, path_of(&a));
        assert!(
            !settings.models_dir_history.contains(&path_of(&a)),
            "当前目录不能留在历史里"
        );

        // A → B：A 进历史——换目录不搬权重，A 下的在册模型仍是商店地盘。
        set_storage_settings(Some(path_of(&b)), None, None).expect("set second models_dir");
        let settings = super::config::load_settings();
        assert_eq!(settings.models_dir, path_of(&b));
        assert!(settings.models_dir_history.contains(&path_of(&a)));

        // B → 换回 A：A 从历史里清掉（它现在是当前值），B 进历史。
        set_storage_settings(Some(path_of(&a)), None, None).expect("switch back");
        let settings = super::config::load_settings();
        assert!(settings.models_dir_history.contains(&path_of(&b)));
        assert!(
            !settings.models_dir_history.contains(&path_of(&a)),
            "换回旧目录后它由当前值覆盖，历史里不该有第二份"
        );

        // 保护链路端到端：当前是 A、历史有 B 时，store_weights_roots 同时
        // 覆盖两个目录（历史目录的守卫面见 boundary 的直打测试）。
        set_storage_settings(Some(path_of(&b)), None, None).expect("set to b again");
        let roots = crate::features::model_store::store_weights_roots();
        assert!(roots.contains(&std::path::PathBuf::from(&path_of(&a))));
        assert!(roots.contains(&std::path::PathBuf::from(&path_of(&b))));
    }

    #[test]
    fn models_dir_history_evicts_the_oldest_root_past_the_cap() {
        // 溢出裁剪方向（十九轮评审：`drain(..keep)` 丢最老端此前零钉——把它
        // 改成丢最新端、或把上限改成任意值，现有用例照绿）。钉住：历史恰为
        // 上限长、被挤出的是**最老**的根；被挤出的根同时脱出
        // store_weights_roots 保护集（在册模型只剩名字臂，属既定容量取舍，
        // 见 config 字段注释）。
        let ctx = super::config::TempHome::new();
        let path_of = |p: &std::path::Path| p.to_string_lossy().to_string();
        for index in 0..=(super::config::MAX_MODELS_DIR_HISTORY + 1) {
            let dir = ctx.path().join(format!("weights-{index:03}"));
            set_storage_settings(Some(path_of(&dir)), None, None).expect("set models_dir");
        }
        let cap = super::config::MAX_MODELS_DIR_HISTORY;
        let settings = super::config::load_settings();
        assert_eq!(
            settings.models_dir_history.len(),
            cap,
            "历史长度必须钉在 MAX_MODELS_DIR_HISTORY"
        );
        let dir = |index: usize| path_of(&ctx.path().join(format!("weights-{index:03}")));
        // 第 18 次换入挤出的是最老端（000），次老的 001 与最新入史的 016 仍在；
        // 当前值 017 不在历史里。
        assert!(
            !settings.models_dir_history.contains(&dir(0)),
            "最老的根应被挤出历史"
        );
        assert!(settings.models_dir_history.contains(&dir(1)));
        assert!(settings.models_dir_history.contains(&(dir(cap))));
        assert!(!settings.models_dir_history.contains(&dir(cap + 1)));
        // 保护集联动：留存的最新历史根在 store_weights_roots 里，被挤出的
        // 最老根已不在（有意取舍，此断言钉住"溢出即失去路径臂保护"语义）。
        let roots = crate::features::model_store::store_weights_roots();
        assert!(roots.contains(&std::path::PathBuf::from(dir(cap - 1))));
        assert!(!roots.contains(&std::path::PathBuf::from(dir(0))));
    }

    #[test]
    fn removing_scan_dir_records_history_and_keeps_weights_protected() {
        // 2026-11-04 评审 P2 的状态转换（settings 层）：卡片脚本会在扫描目录里
        // 就地复用既有权重，从设置移除目录的那一刻起，若不留痕，目录立即脱出
        // 商店权重根集合——设备管理「名称不同的外部模型记录 + 删除权重」就能
        // 删掉仍被引用的唯一副本。钉住：移除 → 记入 import_scan_dirs_history →
        // store_weights_roots 覆盖；重新挂回 → 由当前值接管、历史退掉。
        let ctx = super::config::TempHome::new();
        let kept = ctx.path().join("scan-kept");
        let removed = ctx.path().join("scan-removed");
        let path_of = |p: &std::path::Path| p.to_string_lossy().to_string();

        set_storage_settings(None, Some(vec![path_of(&kept), path_of(&removed)]), None)
            .expect("set scan dirs");
        let settings = super::config::load_settings();
        assert!(
            settings.import_scan_dirs_history.is_empty(),
            "首挂不该有历史，got: {:?}",
            settings.import_scan_dirs_history
        );

        // 移除其一：被移除的目录入史，留下的不入。
        set_storage_settings(None, Some(vec![path_of(&kept)]), None).expect("remove one scan dir");
        let settings = super::config::load_settings();
        assert_eq!(settings.import_scan_dirs, vec![path_of(&kept)]);
        assert!(
            settings
                .import_scan_dirs_history
                .contains(&path_of(&removed))
        );
        assert!(!settings.import_scan_dirs_history.contains(&path_of(&kept)));
        // 保护集联动：被移除的目录仍在商店权重根集合里（守卫面的删除拒绝由
        // device 侧 e2e 直打）。
        let roots = crate::features::model_store::store_weights_roots();
        assert!(roots.contains(&std::path::PathBuf::from(path_of(&removed))));

        // 重新挂回：由当前值接管，历史里的同一目录退掉（不重复占位）。
        set_storage_settings(None, Some(vec![path_of(&kept), path_of(&removed)]), None)
            .expect("re-add scan dir");
        let settings = super::config::load_settings();
        assert!(
            !settings
                .import_scan_dirs_history
                .contains(&path_of(&removed)),
            "重新挂回的目录不该留在历史里，got: {:?}",
            settings.import_scan_dirs_history
        );

        // trim 形状入史：设置里带首尾空白的登记，入史的是 cleaned 后的形状，
        // 守卫前缀匹配才不失配（第七轮评审 P3 同一把尺子）。
        let blank = ctx.path().join("scan-blank");
        set_storage_settings(
            None,
            Some(vec![format!(" {} ", path_of(&blank)), path_of(&kept)]),
            None,
        )
        .expect("set with whitespace");
        set_storage_settings(None, Some(vec![path_of(&kept)]), None).expect("remove it");
        let settings = super::config::load_settings();
        assert!(settings.import_scan_dirs_history.contains(&path_of(&blank)));
    }

    #[test]
    fn installed_weight_paths_recover_referenced_paths_from_records() {
        use crate::features::model_store::platform::linux::db;

        let ctx = super::config::TempHome::new();
        db::init_db().expect("init store db");
        // 空库 → 空引用集合（设备守卫的该臂恒不命中）。
        let empty = crate::features::model_store::installed_weight_paths_checked()
            .expect("empty store enumerates");
        assert!(empty.is_empty(), "got {empty:?}");

        // 在册卡片的两条约定候选（来源仓库名末段、用户模型名）各落一个真实
        // 目录 → 枚举恢复两条 canonical 路径。
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-refs-1",
            model_name: "my-model",
            card_name: "c-refs-1",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/qwen-weights"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert downloaded model");
        let models_dir = super::config::load_settings().models_path();
        let by_repo = models_dir.join("qwen-weights");
        let by_name = models_dir.join("my-model");
        std::fs::create_dir_all(&by_repo).expect("create repo candidate");
        std::fs::create_dir_all(&by_name).expect("create name candidate");
        let refs = crate::features::model_store::installed_weight_paths_checked()
            .expect("enumerate installed refs");
        assert!(
            refs.contains(&by_repo.canonicalize().expect("canonical repo")),
            "repo-name candidate must be recovered, got {refs:?}"
        );
        assert!(
            refs.contains(&by_name.canonicalize().expect("canonical name")),
            "model-name candidate must be recovered, got {refs:?}"
        );

        // 不存在的候选被 exists() 过滤（与 residual_paths 同一尺子）。
        assert!(
            !refs.iter().any(|p| p.ends_with("absent-candidate")),
            "missing candidates must be filtered, got {refs:?}"
        );

        // 就地复用形态：约定位置是软链 → canonicalize 解析到原位。
        let original = ctx.path().join("legacy-scan").join("hf").join("qwen");
        std::fs::create_dir_all(&original).expect("create original weights");
        #[cfg(unix)]
        std::os::unix::fs::symlink(&original, models_dir.join("linked-repo"))
            .expect("create reuse symlink");
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-refs-2",
            model_name: "linked-model",
            card_name: "c-refs-2",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/linked-repo"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert linked model");
        let refs = crate::features::model_store::installed_weight_paths_checked()
            .expect("enumerate after symlink");
        assert!(
            refs.contains(&original.canonicalize().expect("canonical original")),
            "in-place reuse must recover the ORIGINAL path via the symlink, got {refs:?}"
        );

        // 历史根也参与枚举：换目录不搬权重，老根下的在册候选同样恢复。
        let old_models_dir = models_dir.clone();
        let new_models_dir = ctx.path().join("weights-new");
        crate::features::model_store::set_storage_settings(
            Some(new_models_dir.to_string_lossy().into_owned()),
            None,
            None,
        )
        .expect("switch models_dir");
        // 老根（现入历史）下的既有候选仍被恢复。
        let refs = crate::features::model_store::installed_weight_paths_checked()
            .expect("enumerate with history roots");
        assert!(
            refs.contains(&by_repo.canonicalize().expect("canonical old root")),
            "candidates under the historical models_dir root must still be recovered, got {refs:?}"
        );
        let _ = old_models_dir;
    }

    // 2026-11-05 评审 P2 zhuowp 场景 II：官方卡片脚本按标记文件就地发现用户
    // 自取名的原目录，删除守卫在「目标含标记 + 仍有在册卡片」时拦。标记集合
    // 必须由在册卡片开闸：空库 → 空集合（本臂关闭），有在册卡片 → 全集。
    #[test]
    fn installed_import_markers_gate_on_registered_cards() {
        use crate::features::model_store::platform::linux::db;

        // 生命周期守卫：临时 HOME 必须活到测试结束（`_ctx` 命名绑定保留
        // 作用域内存活、同时让 deny(unused_variables) 不报——Linux CI
        // 2026-11-05 评审 5783998563 P2）。
        let _ctx = super::config::TempHome::new();
        db::init_db().expect("init store db");
        // 空库 → 空集合：没有任何在册卡片就没有未跟踪原目录引用的可能。
        let empty = crate::features::model_store::installed_import_markers_checked()
            .expect("empty store enumerates");
        assert!(empty.is_empty(), "got {empty:?}");

        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-markers-1",
            model_name: "m",
            card_name: "c-markers-1",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("Tongyi-MAI/Z-Image-Turbo"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert downloaded model");
        let markers = crate::features::model_store::installed_import_markers_checked()
            .expect("markers with a registered card");
        assert!(
            markers.contains(&"model_index.json".to_string()),
            "official z-image-turbo discovery marker must be listed, got {markers:?}"
        );
        assert!(
            markers.contains(&"model.safetensors.index.json".to_string()),
            "builtin ornith15/qwen38 discovery marker must be listed, got {markers:?}"
        );
    }

    #[test]
    fn installed_weight_paths_cover_builtin_roots_for_legacy_migration() {
        use crate::features::model_store::platform::linux::db;

        // 旧版迁移组合（2026-11-04 评审 P2）：配置里换过权重目录、两类历史
        // 均不存在，但安装记录仍在——旧默认根（内置家目录根 ~/models）上的
        // 在册引用必须由内置根枚举恢复。
        let ctx = super::config::TempHome::new();
        db::init_db().expect("init store db");
        std::fs::write(
            super::config::config_dir().join("config.json"),
            format!(
                r#"{{"models_dir": "{}"}}"#,
                ctx.path().join("custom-weights").to_string_lossy()
            ),
        )
        .expect("write migrated config");
        let settings = super::config::load_settings();
        assert!(settings.models_dir_history.is_empty());
        assert!(settings.import_scan_dirs_history.is_empty());
        let legacy = crate::platform::paths::user_home_dir()
            .join("models")
            .join("qwen-weights");
        std::fs::create_dir_all(&legacy).expect("create legacy weights");
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-legacy",
            model_name: "legacy-model",
            card_name: "c-legacy",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/qwen-weights"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert legacy model");
        let refs = crate::features::model_store::installed_weight_paths_checked()
            .expect("enumerate legacy refs");
        assert!(
            refs.contains(&legacy.canonicalize().expect("canonical legacy")),
            "builtin-root legacy weights must be recovered without any history, got {refs:?}"
        );
        // 自定义新根下的同名候选不存在 → 不误报。
        assert!(
            !refs
                .iter()
                .any(|p| p.starts_with(ctx.path().join("custom-weights"))),
            "missing candidates under the new custom root must not appear, got {refs:?}"
        );
    }

    #[test]
    fn installed_weight_candidate_names_derive_from_records_without_any_root() {
        use crate::features::model_store::platform::linux::db;

        let ctx = super::config::TempHome::new();
        db::init_db().expect("init store db");
        // 空库 → 空名字集合（设备守卫的名字臂恒不命中）。
        let empty = crate::features::model_store::installed_weight_candidate_names_checked()
            .expect("empty store enumerates");
        assert!(empty.is_empty(), "got {empty:?}");

        // 两臂约定名都派生：来源仓库名末段 + 用户模型名。**不依赖任何根、
        // 不做存在性过滤**——这是它能覆盖零痕迹旧根的原因（与
        // installed_weight_paths_checked 的 exists() 尺子方向相反）。
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-names-1",
            model_name: "my-model",
            card_name: "c-names-1",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/qwen-weights"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert downloaded model");
        let names = crate::features::model_store::installed_weight_candidate_names_checked()
            .expect("derive names");
        assert_eq!(
            names,
            vec!["qwen-weights".to_string(), "my-model".to_string()]
        );
        // 盘上一个候选目录都不存在，名字集合照样返回（零痕迹语义）。
        assert!(!ctx.path().join("qwen-weights").exists());

        // 去重 + 防逃逸：第二条记录两臂同名（去重），第三条来源仓库名末段是
        // `..`（safe_path_component 拒绝 → 该臂丢弃），模型名带非法字符
        // （同拒）——名字集合不得出现任何可逃逸形状。
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-names-2",
            model_name: "qwen-weights",
            card_name: "c-names-2",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/qwen-weights"),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert duplicate-name model");
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-names-3",
            model_name: "../escape",
            card_name: "c-names-3",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: Some("org/.."),
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert escaping model");
        let names = crate::features::model_store::installed_weight_candidate_names_checked()
            .expect("derive names after dup and escape");
        assert_eq!(
            names,
            vec!["qwen-weights".to_string(), "my-model".to_string()]
        );
        assert!(
            !names.iter().any(|name| name.contains("..")),
            "escape shapes must be filtered, got {names:?}"
        );
        let _ = ctx;
    }

    #[test]
    fn installed_weight_filenames_derive_from_records_without_paths() {
        use crate::features::model_store::platform::linux::db;

        let ctx = super::config::TempHome::new();
        db::init_db().expect("init store db");
        // 空库 → 空集合（设备守卫的第四恢复臂恒不命中）。
        let empty = crate::features::model_store::installed_weight_filenames_checked()
            .expect("empty store enumerates");
        assert!(empty.is_empty(), "got {empty:?}");

        // 记录里的 source_filename 是唯一输入：None 与空串跳过，合法名保留。
        let insert = |card_id: &str, filename: Option<&str>| {
            db::add_store_model(db::AddStoreModelParams {
                card_id,
                model_name: "renamed-model",
                card_name: card_id,
                org: None,
                description: None,
                engine_name: "e",
                engine_version: None,
                size_bytes: 0,
                quantization: None,
                source_repo_id: None,
                source_filename: filename,
                scripts: &std::collections::BTreeMap::new(),
                status: "downloaded",
                profiles: &[],
                deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
            })
            .expect("insert model");
        };
        insert("c-fn-1", Some("model-Q4_K_M.gguf"));
        insert("c-fn-2", None);
        let names = crate::features::model_store::installed_weight_filenames_checked()
            .expect("derive filenames");
        assert_eq!(names, vec!["model-Q4_K_M.gguf".to_string()]);

        // 去重 + 防逃逸 + 空串跳过：第二张卡同名（去重），第三张带路径分量
        // （safe_path_component 拒绝 → 丢弃），第四张空白串（跳过）。
        insert("c-fn-5", Some("model-Q4_K_M.gguf"));
        insert("c-fn-6", Some("sub/model-Q4_K_M.gguf"));
        insert("c-fn-7", Some("   "));
        let names = crate::features::model_store::installed_weight_filenames_checked()
            .expect("derive filenames after dup/escape/blank");
        assert_eq!(names, vec!["model-Q4_K_M.gguf".to_string()]);
        let _ = ctx;
    }

    #[test]
    fn installed_ownership_unconfirmable_tracks_fully_blind_cards() {
        // 第五臂收窄版（2026-11-08 评审 P2-3）：只有恢复腿全部无数据的卡片
        // （无 source_filename、仓库末段与模型名都过不了 safe_path_component、
        // .env 也没有 MODEL_PATH）才令归属无法确认；任一腿可用（哪怕只是约定
        // 名）都不触发——本臂不放大拒绝面（与名字臂「宁可漏」边界处方一致）。
        use crate::features::model_store::platform::linux::db;

        let ctx = super::config::TempHome::new();
        db::init_db().expect("init store db");
        let probe = || {
            crate::features::model_store::installed_ownership_unconfirmable_checked()
                .expect("probe ownership confirmability")
        };
        assert!(!probe(), "empty store must be confirmable");

        let insert = |card_id: &str, filename: Option<&str>| {
            db::add_store_model(db::AddStoreModelParams {
                card_id,
                model_name: "some-model",
                card_name: card_id,
                org: None,
                description: None,
                engine_name: "e",
                engine_version: None,
                size_bytes: 0,
                quantization: None,
                source_repo_id: Some("org/repo"),
                source_filename: filename,
                scripts: &std::collections::BTreeMap::new(),
                status: "downloaded",
                profiles: &[],
                deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
            })
            .expect("insert model");
        };
        // 有合法文件名 / 有约定名（仓库末段或模型名）的卡片：可确认。
        insert("c-own-1", Some("model.gguf"));
        insert("c-own-2", None);
        assert!(
            !probe(),
            "cards with any usable recovery leg must stay confirmable"
        );
        // 真盲行：无文件名 + 无仓库 + model_name 过不了 safe_path_component
        // （中文自取名/路径分量形状）——登记入口会拒，走原始 SQL 注入
        // （db::connect 私有，按 db_path 直开）。
        let conn = rusqlite::Connection::open(db::db_path()).expect("open store db");
        conn.execute(
            "INSERT INTO store_models
                 (card_id, model_name, card_name, engine_name, status, downloaded_at)
             VALUES ('c-blind-1', '中文自取名', 'cb', 'e', 'downloaded', '2026-01-01'),
                    ('c-blind-2', '..', 'cb2', 'e', 'downloaded', '2026-01-01')",
            [],
        )
        .expect("insert fully blind rows");
        drop(conn);
        assert!(
            probe(),
            "a fully blind card must make ownership unconfirmable"
        );
        // .env 落盘的 MODEL_PATH 是恢复数据：真盲行带上它即恢复可确认
        // （MODEL_PATH 指向的目录必须真实存在——exists() 过滤不产生假引用）。
        for (card_id, weight_name) in [
            ("c-blind-1", "renamed-weights"),
            ("c-blind-2", "other-weights"),
        ] {
            let weights = ctx.path().join(weight_name);
            std::fs::create_dir_all(&weights).expect("create weights dir");
            let card_dir = super::config::data_dir()
                .join("cards")
                .join(card_id)
                .join("vendor");
            std::fs::create_dir_all(&card_dir).expect("create card vendor dir");
            std::fs::write(
                card_dir.join(".env"),
                format!("MODEL_PATH={}\n", weights.to_string_lossy()),
            )
            .expect("write card env");
        }
        assert!(
            !probe(),
            "a blind card with an .env MODEL_PATH must stay confirmable"
        );
        // 卸载（删行）即放行的解锁路径真实存在。
        db::delete_store_model("c-blind-1").expect("remove blind card 1");
        db::delete_store_model("c-blind-2").expect("remove blind card 2");
        assert!(
            !probe(),
            "uninstalling blind cards must restore confirmability"
        );
        let _ = ctx;
    }

    #[test]
    fn parse_env_model_path_takes_the_last_assignment() {
        // 解析口径：支持 export 前缀与成对引号；多次赋值取最后一次（与卡片
        // 脚本 sed -i 覆盖写语义一致）；空值/缺键返回 None。
        assert_eq!(
            super::parse_env_model_path("MODEL_PATH=/home/u/weights\n"),
            Some("/home/u/weights".to_string())
        );
        assert_eq!(
            super::parse_env_model_path(
                "export MODEL_PATH=\"/home/u/my weights\"\nCACHE_PATH=/tmp\n"
            ),
            Some("/home/u/my weights".to_string())
        );
        assert_eq!(
            super::parse_env_model_path("MODEL_PATH=/old\nMODEL_PATH=/new\n"),
            Some("/new".to_string())
        );
        assert_eq!(super::parse_env_model_path("MODEL_PATH=\n"), None);
        assert_eq!(super::parse_env_model_path("CACHE_PATH=/tmp\n"), None);
    }

    #[test]
    fn card_env_model_paths_recover_renamed_weight_dirs() {
        // .env 恢复腿端到端（2026-11-08 评审 P2-3 处方一）：改名目录/零痕迹
        // 旧根上的卡片权重由 .env 的 MODEL_PATH 恢复进 installed refs——
        // 存在性过滤 + canonicalize 解析软链，与约定候选同一把尺。
        use crate::features::model_store::platform::linux::db;

        let ctx = super::config::TempHome::new();
        db::init_db().expect("init store db");
        db::add_store_model(db::AddStoreModelParams {
            card_id: "c-env-1",
            model_name: "some-model",
            card_name: "c-env-1",
            org: None,
            description: None,
            engine_name: "e",
            engine_version: None,
            size_bytes: 0,
            quantization: None,
            source_repo_id: None,
            source_filename: None,
            scripts: &std::collections::BTreeMap::new(),
            status: "downloaded",
            profiles: &[],
            deploy_topology: crate::features::model_store::platform::linux::store_client::DeployTopology::default(),
            card_source: "store",
        })
        .expect("insert model");
        // 约定候选（some-model 目录）不存在，集合为空。
        let empty = crate::features::model_store::installed_weight_paths_checked()
            .expect("enumerate installed refs");
        assert!(empty.is_empty(), "no candidates on disk, got {empty:?}");
        // 卡片 .env 记录的改名目录（零痕迹旧根形状）恢复进集合。
        let renamed = ctx.path().join("zero-trace-root").join("renamed-weights");
        std::fs::create_dir_all(&renamed).expect("create renamed weights");
        let card_dir = super::config::data_dir()
            .join("cards")
            .join("c-env-1")
            .join("vendor");
        std::fs::create_dir_all(&card_dir).expect("create card vendor dir");
        std::fs::write(
            card_dir.join(".env"),
            format!("MODEL_PATH={}\n", renamed.to_string_lossy()),
        )
        .expect("write card env");
        let refs = crate::features::model_store::installed_weight_paths_checked()
            .expect("enumerate installed refs with env leg");
        let resolved = renamed.canonicalize().expect("resolve renamed dir");
        assert!(
            refs.contains(&resolved),
            ".env MODEL_PATH must be recovered, got {refs:?}"
        );
        let unregister_refs =
            crate::features::model_store::installed_weight_paths_for_removal_checked(false)
                .expect("unregister-only keeps main's legacy reference guards");
        assert!(
            unregister_refs.contains(&resolved),
            "the legacy .env reference guard must remain active without physical deletion"
        );
        // 相对路径与不存在的路径不产生假引用。
        std::fs::write(
            card_dir.join(".env"),
            "MODEL_PATH=relative/dir\nMODEL_PATH=/gone/dir\n",
        )
        .expect("write bad env");
        let refs = crate::features::model_store::installed_weight_paths_checked()
            .expect("enumerate installed refs with bad env");
        assert!(
            !refs
                .iter()
                .any(|p| p.ends_with("relative/dir") || p.ends_with("gone/dir")),
            "relative and missing paths must be filtered, got {refs:?}"
        );
        // 旧写入约定（2026-11-09 评审 P2-3，review 5820385522）：前代 LMM 的
        // 数据根 `~/.local/share/local-model-manager/cards/<id>/.env` 同样是
        // 权威落点（config.rs 记载老根在设备上原样保留）。
        let old_card_env = ctx
            .path()
            .join(".local/share/local-model-manager/cards/c-env-1");
        std::fs::create_dir_all(&old_card_env).expect("create legacy card dir");
        let legacy_weights = ctx.path().join("lmm-era-weights");
        std::fs::create_dir_all(&legacy_weights).expect("create legacy weights");
        std::fs::write(card_dir.join(".env"), "MODEL_PATH=/gone/dir\n").expect("write bad env");
        std::fs::write(
            old_card_env.join(".env"),
            format!("MODEL_PATH={}\n", legacy_weights.to_string_lossy()),
        )
        .expect("write legacy card env");
        let refs = crate::features::model_store::installed_weight_paths_checked()
            .expect("enumerate installed refs with legacy root probe");
        let legacy_resolved = legacy_weights
            .canonicalize()
            .expect("resolve legacy weights");
        assert!(
            refs.contains(&legacy_resolved),
            "legacy LMM card .env must be recovered, got {refs:?}"
        );
        let _ = ctx;
    }

    #[test]
    fn import_scan_dirs_history_evicts_the_oldest_root_past_the_cap() {
        // 与 models_dir_history 同一溢出语义：超过 MAX_IMPORT_SCAN_DIRS_HISTORY
        // 后丢最老端，被挤出的根脱出保护集合。
        let ctx = super::config::TempHome::new();
        let path_of = |p: &std::path::Path| p.to_string_lossy().to_string();
        let cap = super::config::MAX_IMPORT_SCAN_DIRS_HISTORY;
        for index in 0..=(cap + 1) {
            let dir = ctx.path().join(format!("scan-{index:03}"));
            set_storage_settings(None, Some(vec![path_of(&dir)]), None).expect("swap scan dir");
        }
        let settings = super::config::load_settings();
        assert_eq!(settings.import_scan_dirs_history.len(), cap);
        let dir = |index: usize| path_of(&ctx.path().join(format!("scan-{index:03}")));
        assert!(!settings.import_scan_dirs_history.contains(&dir(0)));
        assert!(settings.import_scan_dirs_history.contains(&dir(1)));
        assert!(settings.import_scan_dirs_history.contains(&(dir(cap))));
        // 溢出即失去路径臂保护（既定容量取舍，钉住语义）。
        let roots = crate::features::model_store::store_weights_roots();
        assert!(!roots.contains(&std::path::PathBuf::from(dir(0))));
        assert!(roots.contains(&std::path::PathBuf::from(dir(cap))));
    }

    #[test]
    fn import_scan_dirs_must_be_absolute() {
        let ctx = super::config::TempHome::new();
        // 相对扫描目录的解析随调用方工作目录漂移，设备侧移除守卫的前缀匹配
        // 必然失配（第七轮评审 P3）——与 models_dir 同一把尺子在入口拒绝。
        let error = set_storage_settings(
            None,
            Some(vec![
                ctx.path().join("scan-ok").to_string_lossy().to_string(),
                "relative/dir".to_string(),
            ]),
            None,
        )
        .expect_err("relative import_scan_dir must be rejected");
        assert!(error.contains("绝对路径"), "unexpected error: {error}");
        // 校验先于一切副作用（36 轮评审 P3）：组合保存「合法新权重目录 + 相对
        // 扫描目录」整体报错时，权重目录不得被建出来——配置没写，盘上也不该
        // 多出空目录。校验若排在 mkdir 之后，此断言必红。
        let fresh_weights = ctx.path().join("weights-untouched");
        assert!(!fresh_weights.exists(), "fixture dir must not pre-exist");
        let error = set_storage_settings(
            Some(fresh_weights.to_string_lossy().to_string()),
            Some(vec!["relative/dir".to_string()]),
            None,
        )
        .expect_err("combined save must still be rejected");
        assert!(error.contains("绝对路径"), "unexpected error: {error}");
        assert!(
            !fresh_weights.exists(),
            "failed save must not create the weights directory"
        );
        // 合法路径照常保存，空白项照常滤掉。
        let ok = ctx.path().join("scan-ok").to_string_lossy().to_string();
        let settings =
            set_storage_settings(None, Some(vec![format!("  {ok}  "), String::new()]), None)
                .expect("absolute dirs accepted");
        assert_eq!(settings["import_scan_dirs"], serde_json::json!([ok]));
    }

    /// 「首读瞬态失败、保存端重读已恢复」不得把默认快照固化成配置（二十八轮评审
    /// P1 的剩余分支）。
    ///
    /// 评审点名的时序：普通设置入口首拍宽松读取赶上瞬态 I/O 故障（fd 耗尽、一拍
    /// EIO——过一下自己就好）拿到完整默认值；同一拍里 `save_settings` 闸门的重读
    /// 已恢复，只判 Err/Ok 就把默认快照放行——真实 config.json（自定义
    /// `models_dir` 等未知保护范围）被一次普通开关整体洗成默认，后续严格读取把它
    /// 当合法配置接受，权重根保护失效。修复后两个入口首读走
    /// [`super::config::try_load_settings`]、读不到就不改。权限位（mode 000）构造
    /// 不出这个时序——权限失败到重读时仍在，所以用 config 的测试注入缝受控打出
    /// 「首读 Err → 重读 Ok」，入口打一遍再验证一次恢复后的正常路径（原来是两个
    /// 普通设置入口各打一遍，其中测试商店开关那个随内置测试商店一起拿掉了，
    /// 生产只剩存储入口这一个普通设置入口）。
    #[test]
    fn transient_first_read_failure_must_not_persist_the_default_snapshot() {
        let ctx = super::config::TempHome::new();
        let path = super::config::config_dir().join("config.json");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        // 真实配置：自定义权重目录 + 非默认并发数——两者在默认快照里都会变成
        // 默认值，是"覆盖是否发生"的判别标记。
        let original =
            r#"{"models_dir": "/home/u/ai/shared/weights", "max_concurrent_downloads": 3}"#;
        std::fs::write(&path, original).unwrap();

        // 注入恰好 1 次：入口首读 Err、保存端闸门重读 Ok——正是评审点名的恢复时序。
        super::config::arm_transient_read_failure(1);
        let scan = ctx.path().join("scan-more");
        let error =
            set_storage_settings(None, Some(vec![scan.to_string_lossy().to_string()]), None)
                .expect_err("首读失败不得拿默认快照落盘");
        assert!(
            error.contains("读取当前配置失败"),
            "报错应说明读取失败、未做任何修改：{error}"
        );
        assert_eq!(
            std::fs::read_to_string(&path).unwrap(),
            original,
            "失败的调用不得动真实配置：默认快照落盘就是不可恢复的重置"
        );

        // 注入耗尽后的下一次调用走真实设置：改动生效、自定义 models_dir 与真实
        // 字段不丢——若默认快照曾被固化，这里读到的就是默认值了。
        set_storage_settings(None, Some(vec![scan.to_string_lossy().to_string()]), None)
            .expect("恢复后普通保存放行");
        let settings = super::config::try_load_settings().expect("恢复后严格读取应接受写回的配置");
        assert_eq!(
            settings.models_dir, "/home/u/ai/shared/weights",
            "自定义权重目录不得被默认值覆盖"
        );
        assert_eq!(
            settings.max_concurrent_downloads, 3,
            "真实配置的其他字段原样保留"
        );
        assert_eq!(
            settings.import_scan_dirs,
            vec![scan.to_string_lossy().to_string()],
            "存储入口的改动要落进新配置"
        );
    }
}

#[cfg(test)]
mod durable_history_tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    fn write_run(
        id: &str,
        kind: &str,
        card: &str,
        outcome: Option<Result<Value, String>>,
        epoch: Option<i64>,
    ) -> std::path::PathBuf {
        let path = config::data_dir().join("runs").join(format!("{id}.json"));
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        let saved = SavedRun {
            card_id: card.into(),
            kind: kind.into(),
            created_at: chrono::DateTime::from_timestamp(epoch.unwrap_or(1_577_836_800), 0)
                .unwrap()
                .to_rfc3339(),
            lines: vec!["retained history".into()],
            line_offset: 0,
            progress: None,
            outcome,
            cancelled: false,
            dismissed: false,
            finished_at_epoch: epoch,
            install_context: None,
            retry_reference: None,
        };
        std::fs::write(&path, serde_json::to_vec(&saved).unwrap()).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        path
    }

    #[test]
    fn completed_history_stays_cold_across_restarts_and_keeps_exact_id_replay_and_failure_proofs() {
        let _home = config::TempHome::new();
        db::init_db().unwrap();
        for i in 0..4097 {
            write_run(
                &format!("old-{i}"),
                "stop",
                "history-card",
                Some(Ok(Value::Null)),
                Some(1_577_836_800 + i),
            );
        }
        let failure = write_run(
            "unique-failure",
            "install_local",
            "failed-card",
            Some(Err("original failure".into())),
            Some(1_577_836_800),
        );
        let original = std::fs::read(&failure).unwrap();
        for _ in 0..2 {
            let service = Arc::new(ModelStoreService::new());
            service.restore_runs().unwrap();
            assert!(
                service
                    .runs
                    .lock()
                    .values()
                    .all(|log| log.card_id == "fresh-card"),
                "restart must not heat old terminal logs"
            );
            let cold = service.lookup_run("unique-failure").unwrap().unwrap();
            assert_eq!(*cold.finished_at_epoch.lock(), Some(1_577_836_800));
            assert_eq!(
                cold.outcome.lock().as_ref().unwrap().as_ref().unwrap_err(),
                "original failure"
            );
            let replayed = spawn_op_with_id(
                &service,
                "history-card",
                OP_STOP,
                Some("old-0".into()),
                |_, _, _| panic!("old ID must never execute twice"),
            )
            .unwrap();
            assert_eq!(replayed["run_id"], "old-0");
            let before: SavedRun = serde_json::from_slice(&original).unwrap();
            let after: SavedRun =
                serde_json::from_slice(&std::fs::read(&failure).unwrap()).unwrap();
            assert_eq!(after.outcome, before.outcome);
            assert_eq!(after.created_at, before.created_at);
            assert!(after.lines.is_empty());
            let started = spawn_op(&service, "fresh-card", OP_DOWNLOAD, |_, _, _| {
                Err("fresh failure".into())
            })
            .unwrap();
            let run_id = started["run_id"].as_str().unwrap();
            let deadline = Instant::now() + Duration::from_secs(3);
            while service.run(run_id).unwrap().outcome.lock().is_none() {
                assert!(Instant::now() < deadline);
                std::thread::sleep(Duration::from_millis(5));
            }
            let items = super::super::incomplete::summaries().unwrap();
            assert!(items.iter().any(|item| item["runId"] == "unique-failure"));
            assert!(items.iter().any(|item| item["runId"] == run_id));
        }
    }

    #[test]
    fn legacy_completion_uses_existing_mtime_and_interrupted_runs_get_one_durable_epoch() {
        let _home = config::TempHome::new();
        let path = write_run(
            "legacy",
            "stop",
            "history-card",
            Some(Ok(Value::Null)),
            None,
        );
        let old = std::time::UNIX_EPOCH + Duration::from_secs(1_600_000_000);
        std::fs::File::open(&path)
            .unwrap()
            .set_times(std::fs::FileTimes::new().set_modified(old))
            .unwrap();
        write_run("interrupted", "download", "failed-card", None, None);
        let service = ModelStoreService::new();
        service.restore_runs().unwrap();
        assert!(!service.runs.lock().contains_key("legacy"));
        assert_eq!(
            *service
                .lookup_run("legacy")
                .unwrap()
                .unwrap()
                .finished_at_epoch
                .lock(),
            Some(1_600_000_000)
        );
        let first_epoch = *service.run("interrupted").unwrap().finished_at_epoch.lock();
        assert!(first_epoch.unwrap() >= chrono::Utc::now().timestamp() - 2);
        let restarted = ModelStoreService::new();
        restarted.restore_runs().unwrap();
        assert_eq!(
            *restarted
                .run("interrupted")
                .unwrap()
                .finished_at_epoch
                .lock(),
            first_epoch
        );
        let saved: SavedRun = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert_eq!(saved.finished_at_epoch, Some(1_600_000_000));
        assert!(saved.lines.is_empty());
    }

    #[test]
    fn unknown_and_corrupt_receipts_never_replay_requested_ids() {
        use std::sync::atomic::{AtomicBool, Ordering};
        let _home = config::TempHome::new();
        let service = Arc::new(ModelStoreService::new());
        for (id, kind) in [("unknown", "future-operation"), ("corrupt", "stop")] {
            let path = write_run(
                id,
                kind,
                "history-card",
                Some(Ok(Value::Null)),
                Some(1_577_836_800),
            );
            match id {
                "unknown" => {
                    service.restore_runs().unwrap();
                    assert!(
                        service.runs.lock().is_empty(),
                        "startup may skip an unsupported kind"
                    );
                }
                _ => std::fs::write(&path, b"{").unwrap(),
            }
            let before = std::fs::read(&path).unwrap();
            assert!(
                service.lookup_run(id).is_err(),
                "an existing receipt cannot become absence: {id}"
            );
            let executed = Arc::new(AtomicBool::new(false));
            let execution = executed.clone();
            let result = spawn_op_with_id(
                &service,
                "history-card",
                OP_STOP,
                Some(id.into()),
                move |_, _, _| {
                    execution.store(true, Ordering::SeqCst);
                    Ok(Value::Null)
                },
            );
            assert!(result.is_err(), "must reject retained requested ID: {id}");
            assert!(!executed.load(Ordering::SeqCst));
            assert_eq!(
                std::fs::read(&path).unwrap(),
                before,
                "original receipt must remain byte-for-byte intact"
            );
        }
    }

    #[test]
    fn cold_receipt_read_accepts_whitespace_padded_json() {
        let _home = config::TempHome::new();
        let path = write_run(
            "bounded",
            "stop",
            "history-card",
            Some(Ok(Value::Null)),
            Some(1_577_836_800),
        );
        let mut bytes = std::fs::read(&path).unwrap();
        bytes.resize(16 * 1024 * 1024, b' ');
        std::fs::write(&path, &bytes).unwrap();
        let service = ModelStoreService::new();
        assert!(service.lookup_run("bounded").unwrap().is_some());
        assert_eq!(std::fs::read(path).unwrap(), bytes);
    }

    #[test]
    fn producer_sized_logs_survive_restart_cold_lookup_and_exact_id_replay() {
        let _home = config::TempHome::new();
        let service = Arc::new(ModelStoreService::new());
        let log = service
            .new_run("large-valid", "history-card", "stop")
            .unwrap();
        let line = "x".repeat(1024);
        for _ in 0..17 * 1024 {
            log.push(&line);
        }
        log.finish(Ok(Value::Null), false);
        *log.finished_at_epoch.lock() = Some(1_577_836_800);
        log.persist_result().unwrap();
        let path = config::data_dir().join("runs/large-valid.json");
        let original = std::fs::read(&path).unwrap();
        assert!(
            original.len() < 2 * LOG_TAIL_BYTES,
            "producer checkpoints retain a bounded tail"
        );
        drop(log);
        drop(service);
        for _ in 0..2 {
            let restarted = Arc::new(ModelStoreService::new());
            restarted.restore_runs().unwrap();
            assert!(
                restarted.runs.lock().is_empty(),
                "old complete logs stay cold"
            );
            let cold = restarted.lookup_run("large-valid").unwrap().unwrap();
            assert!(cold.lines.lock().is_empty());
            assert_eq!(*cold.line_offset.lock(), 17 * 1024);
            let replayed = spawn_op_with_id(
                &restarted,
                "history-card",
                OP_STOP,
                Some("large-valid".into()),
                |_, _, _| panic!("large old receipt must not execute twice"),
            )
            .unwrap();
            assert_eq!(replayed["run_id"], "large-valid");
            let saved: SavedRun = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
            assert_eq!(saved.card_id, "history-card");
            assert!(saved.outcome.unwrap().is_ok());
            assert_eq!(saved.line_offset, 17 * 1024);
        }
    }

    #[test]
    fn large_failed_install_logs_keep_preview_and_retry_available_after_restart() {
        let _home = config::TempHome::new();
        let _environment = crate::platform::paths::tests::EnvVarGuard::capture(&["CMSD_SOCKET"]);
        // SAFETY: TempHome holds the shared environment lock for this isolated fixture.
        unsafe {
            std::env::set_var("CMSD_SOCKET", config::data_dir().join("missing-cmsd.sock"));
        }
        db::init_db().unwrap();
        config::save_settings(&config::Settings {
            min_free_space_gb: 0.0,
            ..Default::default()
        })
        .unwrap();
        let card = "large-log-local";
        let directory = config::data_dir().join("cards").join(card);
        std::fs::create_dir_all(&directory).unwrap();
        for entry in [
            "install_engine",
            "download_model",
            "deploy",
            "enable",
            "stop",
            "uninstall",
        ] {
            std::fs::write(
                directory.join(format!("{entry}.sh")),
                "#!/bin/bash\nexit 0\n",
            )
            .unwrap();
        }
        std::fs::write(directory.join("common.sh"), "MODEL_ID=fixture\n").unwrap();
        std::fs::write(
            directory.join("card.json"),
            r#"{"name":"Large log fixture","engine_name":"large-log-fixture","size_bytes":1}"#,
        )
        .unwrap();
        let service = Arc::new(ModelStoreService::new());
        let log = service
            .new_run("large-install-failure", card, "install_local")
            .unwrap();
        log.set_install_context(ModelManager::new(None, None).install_context("install_local"))
            .unwrap();
        let line = "x".repeat(1024);
        for _ in 0..17 * 1024 {
            log.push(&line);
        }
        log.finish(Err("original cancelled install".into()), true);
        *log.finished_at_epoch.lock() = Some(1_577_836_800);
        log.persist_result().unwrap();
        let path = config::data_dir().join("runs/large-install-failure.json");
        let original = std::fs::read(&path).unwrap();
        assert!(original.len() < 2 * LOG_TAIL_BYTES);
        drop(log);
        drop(service);
        let restarted = Arc::new(ModelStoreService::new());
        restarted.restore_runs().unwrap();
        assert!(restarted.runs.lock().is_empty());
        assert_eq!(
            super::super::incomplete::summaries().unwrap()[0]["runId"],
            "large-install-failure"
        );
        let preview = super::super::incomplete::preview("large-install-failure").unwrap();
        let digest = preview.public["previewSha256"]
            .as_str()
            .unwrap()
            .to_string();
        let accepted = retry_incomplete_with_id(
            &restarted,
            "large-install-failure".into(),
            digest,
            Some("cmsd-large-retry".into()),
        )
        .unwrap();
        assert_eq!(accepted["run_id"], "cmsd-large-retry");
        let retry = restarted.run("cmsd-large-retry").unwrap();
        let deadline = Instant::now() + Duration::from_secs(20);
        loop {
            if let Some(outcome) = retry.outcome.lock().clone() {
                assert!(outcome.is_ok(), "{outcome:?}");
                break;
            }
            assert!(Instant::now() < deadline, "local retry did not finish");
            std::thread::sleep(Duration::from_millis(10));
        }
        assert_eq!(
            db::get_store_model(card).unwrap().unwrap().status,
            "deployed"
        );
        let before: SavedRun = serde_json::from_slice(&original).unwrap();
        let after: SavedRun = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert_eq!(after.install_context, before.install_context);
        assert_eq!(after.outcome, before.outcome);
        assert_eq!(after.created_at, before.created_at);
        assert!(after.lines.is_empty());
    }
}

#[cfg(test)]
mod bounded_log_observation_tests {
    use super::*;

    #[tokio::test]
    async fn terminal_log_backlog_and_utf8_fragments_round_trip_without_receipt_changes() {
        let _home = config::TempHome::new();
        let service = Arc::new(ModelStoreService::new());
        let log = service
            .new_run("paged-receipt", "history-card", "stop")
            .unwrap();
        let mut original = vec!["x".repeat(1024); 20_000];
        original.push("猫\0\\\"".repeat(600_000));
        original.push("last original line".into());
        *log.lines.lock() = original.clone();
        log.finish(Ok(json!({"original":true})), false);
        let path = config::data_dir().join("runs/paged-receipt.json");
        let receipt = std::fs::read(&path).unwrap();
        let (mut cursor, mut byte_cursor) = (0, 0);
        let mut restored = Vec::new();
        let mut partial = String::new();
        let mut pages = 0;
        loop {
            let page = run_log(
                Arc::clone(&service),
                "paged-receipt".into(),
                cursor,
                Some(0),
                Some(byte_cursor),
            )
            .await
            .unwrap();
            assert!(serde_json::to_vec(&json!({"result":page})).unwrap().len() < 1024 * 1024);
            assert_eq!(page["done"], true);
            assert_eq!(page["ok"], true);
            assert_eq!(page["result"], json!({"original":true}));
            assert_eq!(page["total"], original.len());
            restored.extend(
                page["lines"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|v| v.as_str().unwrap().to_owned()),
            );
            if let Some(fragment) = page.get("fragment") {
                assert_eq!(fragment["offset"], byte_cursor);
                partial.push_str(fragment["text"].as_str().unwrap());
                if fragment["complete"] == true {
                    restored.push(std::mem::take(&mut partial));
                }
            }
            let next = page["next"].as_u64().unwrap() as usize;
            let next_byte = page["next_byte"].as_u64().unwrap() as usize;
            assert!(next > cursor || (next == cursor && next_byte > byte_cursor));
            (cursor, byte_cursor) = (next, next_byte);
            pages += 1;
            if cursor == original.len() && byte_cursor == 0 {
                break;
            }
        }
        assert!(pages > 3);
        assert!(partial.is_empty());
        assert_eq!(restored, original);
        assert_eq!(std::fs::read(&path).unwrap(), receipt);
    }

    #[tokio::test]
    async fn oversized_errors_and_results_are_explicit_bounded_observations_with_original_receipts()
    {
        let _home = config::TempHome::new();
        let service = Arc::new(ModelStoreService::new());
        let huge = "猫\0".repeat(600_000);
        for (id, outcome) in [
            ("failed", Err(huge.clone())),
            ("success", Ok(json!({"model_name":huge}))),
        ] {
            let log = service.new_run(id, "history-card", "stop").unwrap();
            *log.lines.lock() = vec![huge.clone(), "last line".into()];
            log.finish(outcome, false);
            let path = config::data_dir().join("runs").join(format!("{id}.json"));
            let before = std::fs::read(&path).unwrap();
            let page = run_log(Arc::clone(&service), id.into(), 0, Some(0), None)
                .await
                .unwrap();
            assert!(serde_json::to_vec(&json!({"result":page})).unwrap().len() < 1024 * 1024);
            assert_eq!(page["done"], true);
            assert_eq!(page["ok"], id == "success");
            assert_eq!(page["error_truncated"], id == "failed");
            assert_eq!(page["result_truncated"], id == "success");
            let snapshot = log.to_task_snapshot(id, 500);
            assert!(snapshot.lines_truncated);
            assert_eq!(snapshot.error_truncated, id == "failed");
            assert_eq!(snapshot.lines.last().unwrap(), "last line");
            assert!(
                serde_json::to_vec(&snapshot.lines).unwrap().len()
                    < super::super::log_page::MAX_PAGE_BYTES
            );
            assert_eq!(std::fs::read(path).unwrap(), before);
        }
    }
}
