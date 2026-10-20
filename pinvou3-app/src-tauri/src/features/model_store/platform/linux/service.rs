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
    OP_FORCE_REMOVE, OP_STOP, OP_UNINSTALL, StartupCleanup,
};
use super::{config, db, disk, import_scan, script_runner};

/// 运行记录保留多久。一次下载的日志能有几万行，不清会一直涨。
///
/// 到点之后**内存里**这条运行整个消失（`sweep_old_runs`）。日志本身不会跟着没——
/// 它已经由 `run_log_store` 落在 `~/.pinvou3/model-store/logs/` 下了，这正是加那个
/// 模块的原因：在它之前，600 秒一过「昨天那次下载为什么失败」在设备上无法回答。
const RUN_RETENTION: Duration = Duration::from_secs(600);

/// 内存里最多留多少行。
///
/// 在这之前 `RunLog::lines` 是**无上限**的 `Vec`：一次 168GB 的下载能打出几万行，
/// 连同 `script_runner` 那份 `output_lines` 一起常驻，而它们在 600 秒的保留期里
/// 一直不释放。封顶之后丢掉的是最老的行，完整的一份在磁盘上。
const MAX_MEMORY_LINES: usize = 5_000;
/// 长轮询最多挂多久。跟前端的 `LOG_WAIT_SECONDS` 对齐。
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

/// 有上限的行缓冲，**同时保住长轮询的绝对游标语义**。
///
/// 封顶不能简单地 `lines.drain(..n)`：前端握着的 `from` 是**绝对序号**，重排之后
/// 同一个游标会指到别的行上——界面会把一段日志重收一遍，而在有 `LMM_PROMPT=` 的
/// 路径上"重收一遍"等于把答过的提示再弹一次。所以丢掉的行数记在 `base` 里，
/// 序号只增不减；游标落在已丢掉的区间时，从现存最老的那一行开始给。
struct LineRing {
    /// `items[0]` 在整条运行里的绝对序号。
    base: usize,
    items: std::collections::VecDeque<String>,
}

impl LineRing {
    fn new() -> Self {
        Self {
            base: 0,
            items: std::collections::VecDeque::new(),
        }
    }

    fn push(&mut self, line: &str) {
        self.items.push_back(line.to_string());
        while self.items.len() > MAX_MEMORY_LINES {
            self.items.pop_front();
            self.base += 1;
        }
    }

    /// 整条运行到目前为止一共有过多少行（= 下一个游标）。
    fn total(&self) -> usize {
        self.base + self.items.len()
    }

    fn slice_from(&self, from: usize) -> Vec<String> {
        let offset = from.saturating_sub(self.base);
        if offset >= self.items.len() {
            return Vec::new();
        }
        self.items.iter().skip(offset).cloned().collect()
    }
}

/// 一次长操作的输出与结局。
///
/// 行存在内存里而不是只往外推：调用方可能晚到（界面切走再切回来、应用重启），
/// 没有回放就会丢掉前半段日志，而前半段往往正是出错的地方。
///
/// 内存那份有上限（`MAX_MEMORY_LINES`）且 600 秒后整条消失；**完整的一份落在磁盘
/// 上**（`disk`）。两份的分工：内存那份服务于"现在正在看的界面"，磁盘那份服务于
/// "明天再来问为什么失败的人"。
struct RunLog {
    lines: Mutex<LineRing>,
    /// 落盘句柄。**开不出来就是 `None`，操作照常跑**——日志是诊断手段，不是功能，
    /// 为了记日志而让用户下不了模型是本末倒置。
    disk: Option<RunLogFile>,
    /// Docker 层的逐帧刷新不落盘时，留一条说明，避免诊断人员以为日志突然缺了一段。
    docker_progress_notice_emitted: Mutex<bool>,
    /// `None` = 还在跑；`Some(Ok(v))` = 成功及其返回值；`Some(Err(msg))` = 失败
    outcome: Mutex<Option<Result<Value, String>>>,
    /// 用户主动取消。取消在底层也是一个 Err，但对用户是两回事——"你点了取消"
    /// 和"它炸了"不该长得一样，调用方要能分别给出提示。
    cancelled: Mutex<bool>,
    /// 现在是不是有一条脚本提示在等人作答。
    ///
    /// 界面把「要不要弹输入框」从日志行里解析出来，而日志是可回放的——切走再
    /// 回来会从头读一遍。答过的提示被回放时必须**不能**再弹：用户填的答案会写进
    /// 一个没人来取的应答槽，下一次真正的提问一出现就被这份陈答案直接顶掉，
    /// 脚本于是拿着错误的输入继续往下跑（双机部署里那就是拿旧 IP 去连从机）。
    /// 所以"还在不在等"得由后端说了算，它是唯一知道真相的一方。
    awaiting_prompt: Mutex<bool>,
    finished_at: Mutex<Option<Instant>>,
    /// 有新行或结束时唤醒正在长轮询的调用
    notify: Arc<tokio::sync::Notify>,
}

impl RunLog {
    fn new(run_id: &str, label: &str) -> Self {
        Self {
            lines: Mutex::new(LineRing::new()),
            disk: RunLogFile::open(run_id, label),
            docker_progress_notice_emitted: Mutex::new(false),
            outcome: Mutex::new(None),
            cancelled: Mutex::new(false),
            awaiting_prompt: Mutex::new(false),
            finished_at: Mutex::new(None),
            notify: Arc::new(tokio::sync::Notify::new()),
        }
    }

    fn push(&self, line: &str) {
        // 提示行之后脚本就阻塞在 read 上，再出现任何一行输出就说明它已经拿到
        // 答案往下走了——这是日志流里唯一能看出"还在不在等"的地方。
        *self.awaiting_prompt.lock() = is_prompt_line(line);
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
        self.lines.lock().push(line);
        self.notify.notify_waiters();
    }

    /// 只在内存里的 RunLog，给测试用。
    ///
    /// 不让测试走 `new()`：那会在开发机/CI 的**真实** `~/.pinvou3/model-store/logs/`
    /// 下留文件。落盘本身有专门的文件级测试（`run_log_store`），用 `TempHome` 隔离。
    #[cfg(test)]
    fn new_memory_only() -> Self {
        Self {
            lines: Mutex::new(LineRing::new()),
            disk: None,
            docker_progress_notice_emitted: Mutex::new(false),
            outcome: Mutex::new(None),
            cancelled: Mutex::new(false),
            awaiting_prompt: Mutex::new(false),
            finished_at: Mutex::new(None),
            notify: Arc::new(tokio::sync::Notify::new()),
        }
    }

    /// 界面答完了。不等脚本打出下一行就落下标志：用户完全可能答完立刻切走，
    /// 那一瞬间再回来的话，回放会把同一条提示又弹一次。
    fn answered(&self) {
        *self.awaiting_prompt.lock() = false;
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
        *self.finished_at.lock() = Some(Instant::now());
        self.notify.notify_waiters();
    }

    fn snapshot(&self, from: usize) -> (Vec<String>, usize, Option<Result<Value, String>>) {
        let lines = self.lines.lock();
        (
            lines.slice_from(from),
            lines.total(),
            self.outcome.lock().clone(),
        )
    }
}

// ---------------------------------------------------------------- 服务状态

/// 模型商店的进程内状态。作为 Tauri 的 managed state 注册一次。
///
/// `active` / `queue` 必须是**全进程唯一**的：它们是卡片级互斥和下载并发的唯一
/// 依据，各持一份等于没有约束。真机上踩过——两个 `hf download` 抢同一批文件的锁，
/// 速度从 2.2MB/s 掉到 569KB/s。
pub struct ModelStoreService {
    registry: CancelRegistry,
    active: Arc<ActiveCardOps>,
    queue: Arc<DownloadQueue>,
    runs: Mutex<HashMap<String, Arc<RunLog>>>,
    startup_cleanup: StartupCleanup,
}

impl Default for ModelStoreService {
    fn default() -> Self {
        Self::new()
    }
}

impl ModelStoreService {
    pub fn new() -> Self {
        Self {
            registry: CancelRegistry::default(),
            active: Arc::new(ActiveCardOps::default()),
            queue: Arc::new(DownloadQueue::default()),
            runs: Mutex::new(HashMap::new()),
            startup_cleanup: StartupCleanup::default(),
        }
    }

    fn new_run(&self, run_id: &str, label: &str) -> Arc<RunLog> {
        self.sweep_old_runs();
        let log = Arc::new(RunLog::new(run_id, label));
        self.runs
            .lock()
            .insert(run_id.to_string(), Arc::clone(&log));
        log
    }

    fn run(&self, run_id: &str) -> Option<Arc<RunLog>> {
        self.runs.lock().get(run_id).cloned()
    }

    fn sweep_old_runs(&self) {
        let now = Instant::now();
        self.runs.lock().retain(|_, log| {
            log.finished_at
                .lock()
                .is_none_or(|t| now.duration_since(t) < RUN_RETENTION)
        });
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
fn needs_redeploy(record: &db::StoreModelRecord) -> bool {
    if record.status != "deployed" {
        return false;
    }
    if let (Some(deployed), Some(current)) = (
        record.deployed_script_sha256.as_deref(),
        record
            .scripts_content
            .get("deploy")
            .map(|b| b.sha256.as_str()),
    ) {
        return deployed != current;
    }
    // 老记录没有部署时的哈希（这个字段是后加的）。退而用时间戳判断：脚本副本
    // 是在部署**之后**才抓下来的，就说明部署用的是更老的脚本。两个时间戳都是
    // 同一个 now_iso() 生成的 RFC3339，按字典序比较就是按时间比较。
    match (
        record.scripts_fetched_at.as_deref(),
        record.deployed_at.as_deref(),
    ) {
        (Some(fetched), Some(deployed_at)) => fetched > deployed_at,
        // 信息不足时一律不提示。宁可少提醒一次，也不要凭空冒出一个「重新部署」
        // 让用户莫名其妙。
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
fn library_payload(models: Vec<db::StoreModelRecord>) -> Value {
    // 先算两个派生判定再瘦身：它们要读 scripts_content，而 strip 会摘掉它。
    let flags: Vec<bool> = models.iter().map(needs_redeploy).collect();
    // 本地脚本哈希摘要（script_type -> sha256）。前端拿它跟卡片列表的
    // script_sha256 比对，自动检测「服务器有更新」——用户不需要手动检查。
    let local_hashes: Vec<Value> = models
        .iter()
        .map(|m| {
            let mut map = serde_json::Map::new();
            for (kind, blob) in &m.scripts_content {
                map.insert(kind.clone(), Value::String(blob.sha256.clone()));
            }
            Value::Object(map)
        })
        .collect();
    let mut value = strip_bulky_fields(models);
    if let Some(items) = value.as_array_mut() {
        for (i, item) in items.iter_mut().enumerate() {
            if let Some(obj) = item.as_object_mut() {
                obj.insert("needs_redeploy".to_string(), Value::Bool(flags[i]));
                obj.insert("local_script_hashes".to_string(), local_hashes[i].clone());
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
}

impl Drop for OpCleanup {
    fn drop(&mut self) {
        self.registry_owner.registry.remove(&self.run_id);
        self.registry_owner.active.finish(&self.card);
        // 下载之外的操作没进过队列，release 对它们是空操作
        self.registry_owner.queue.release(&self.run_id);
        // finish 必须是最后一步：它一置位，长轮询就会立刻带着结局返回，调用方
        // 可能下一毫秒就来重试同一张卡——那时互斥必须已经放开了。
        let outcome = self
            .outcome
            .take()
            .unwrap_or_else(|| Err("内部错误：任务线程异常终止，没有留下结果".to_string()));
        self.log.finish(outcome, self.control.cancelled());
    }
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
    // run_id 要在占坑之前先发出来：互斥登记里存着它，界面切走再回来时正是按
    // 卡片把它要回去接着读日志的（见 `ActiveCardOps::run_for_card`）。
    // 但登记表之外的东西一律等占坑成功再建——占不到坑就直接返回，不能留下一条
    // 没人会去读、也没人会去收的运行记录。
    let run_id = new_run_id();
    let card_guard = super::runtime_guard::StartupGuard::for_card(card_id)?;
    // 卡片级互斥：同一张卡不许被两处同时操作。
    service.active.try_begin(card_id, op, Some(&run_id))?;

    let log = service.new_run(&run_id, op.label);
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
                    Err(format!(
                        "{line}。这是模型商店自身的缺陷，请把日志反馈给我们；\
                     该模型现在可以重新操作。"
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

pub async fn library() -> Result<Value, String> {
    tokio::task::spawn_blocking(|| {
        let mgr = ModelManager::new(None, None);
        // 先按实际容器状态纠正一次 running：设备可能是重启之后才起的应用，
        // 库里的 running 标记这时候是过期的。
        let corrected = mgr.reconcile_running_status();
        let models = db::list_store_models().map_err(|e| e.to_string())?;
        Ok(json!({
            "models": library_payload(models),
            "corrected": corrected,
        }))
    })
    .await
    .map_err(|e| e.to_string())?
}

pub async fn model_detail(card_id: String) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || {
        ModelManager::new(None, None)
            .get_model_detail(&card_id)
            .map_err(|e| e.to_string())
            .and_then(|d| serde_json::to_value(d).map_err(|e| e.to_string()))
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
pub async fn memory_admission(card_id: String) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || {
        ModelManager::new(None, None)
            .memory_admission(&card_id)
            .map_err(|e| e.to_string())
            .and_then(|admission| serde_json::to_value(admission).map_err(|e| e.to_string()))
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

// ---------------------------------------------------------------- 长操作

pub fn start_download(service: &Arc<ModelStoreService>, card_id: String) -> Result<Value, String> {
    let card_for_job = card_id.clone();
    let queue = Arc::clone(&service.queue);

    spawn_op(
        service,
        &card_id,
        OP_DOWNLOAD,
        move |log, control, run_id| {
            // 取号要在真正开跑之前：用户点下载的先后顺序就是排队顺序，等装完引擎再
            // 取号会让装引擎快的插到慢的前面。
            let ticket = queue.enqueue(run_id);
            let log_gate = Arc::clone(log);
            let control_gate = control.clone();
            let mut announced = false;
            let mut gate = move || -> Result<(), super::errors::AppError> {
                loop {
                    if control_gate.cancelled() {
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
                        log_gate.push(&format!(
                            "引擎已就绪。当前有模型正在下载，本任务排队中（前面还有 {ahead} 个）\
                         ——可以放着不管，轮到它会自动开始下载"
                        ));
                    }
                    std::thread::sleep(Duration::from_millis(500));
                }
            };
            let log_cb = Arc::clone(log);
            let mut cb = move |line: &str| log_cb.push(line);
            ModelManager::new(None, None)
                .download_from_store(&card_for_job, &mut cb, control, Some(&mut gate))
                .map(|r| serde_json::to_value(r).unwrap_or(Value::Null))
                .map_err(|e| e.to_string())
        },
    )
}

pub fn start_card_scripts(
    service: &Arc<ModelStoreService>,
    card_id: String,
    rollback: bool,
    confirmed: bool,
) -> Result<Value, String> {
    if !confirmed {
        return Err("更新或回退卡片脚本需要明确确认；不会下载或删除模型权重。".into());
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
            .change_card_scripts(&card, rollback, &control)
            .map_err(|e| e.to_string())?;
        log.push(if changed {
            "脚本包已安全保存。已部署模型请重新部署后再启用；如有问题可回退上次脚本。"
        } else {
            "本地脚本包已是当前版本，没有修改。"
        });
        Ok(json!({"changed": changed, "rollback": rollback}))
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
    let op = match action.as_str() {
        "enable" => OP_ENABLE,
        "stop" => OP_STOP,
        "uninstall" => OP_UNINSTALL,
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
    if op == OP_UNINSTALL {
        if let Ok(Some(rec)) = db::get_store_model(&card_id) {
            if rec.running {
                return Err(format!(
                    "「{}」正在运行，请先停用再卸载。直接卸载会在服务还没停干净时\
                     就删掉权重和库记录，之后那个还在跑的进程就没有任何界面能停得掉了。",
                    rec.card_name
                ));
            }
        }
    }

    let card = card_id.clone();
    spawn_op(service, &card_id, op, move |log, control, _run_id| {
        let _startup_guard = startup_guard;
        let log_cb = Arc::clone(log);
        let mut cb = move |line: &str| log_cb.push(line);
        let mgr = ModelManager::new(None, None);
        match action.as_str() {
            "enable" => mgr
                .enable(&card, &mut cb, control)
                .map(|_| Value::Null)
                .map_err(|e| e.to_string()),
            "stop" => mgr
                .stop(&card, &mut cb, control)
                .map(|_| Value::Null)
                .map_err(|e| e.to_string()),
            _ => mgr
                .uninstall(&card, purge_weights, &mut cb, control)
                .map(|outcome| serde_json::to_value(outcome).unwrap_or(Value::Null))
                .map_err(|e| e.to_string()),
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
) -> Result<Value, String> {
    let log = service
        .run(&run_id)
        .ok_or_else(|| "没有这个运行记录（可能已过期）".to_string())?;

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

        let (lines, next, outcome) = log.snapshot(from);
        // 有新行、或者已经结束，就立刻返回；否则挂着等唤醒
        if !lines.is_empty() || outcome.is_some() || Instant::now() >= deadline {
            let (done, ok, result, error) = match outcome {
                None => (false, false, Value::Null, Value::Null),
                Some(Ok(v)) => (true, true, v, Value::Null),
                Some(Err(e)) => (true, false, Value::Null, Value::String(e)),
            };
            return Ok(json!({
                "lines": lines,
                "next": next,
                "done": done,
                "ok": ok,
                "cancelled": *log.cancelled.lock(),
                // 「现在还有没有提示在等人答」。调用方从日志行里解析提示，而日志
                // 是可回放的——没有这个标志，界面切走再回来就会把早已答过的提示
                // 再弹一次，用户填的答案落进一个没人来取的槽，下一次真正的提问
                // 会被这份陈答案顶掉。
                "awaiting_input": *log.awaiting_prompt.lock(),
                "result": result,
                "error": error,
            }));
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
    std::fs::create_dir_all(&dir).map_err(|e| format!("创建日志目录失败：{e}"))?;
    crate::platform::os::reveal_target(&dir)?;
    Ok(json!({ "path": dir.to_string_lossy() }))
}

/// 导出一次运行的日志（比落盘那份多过一道脱敏，见 `run_log_store`）。
pub fn export_run_log(run_id: String) -> Result<Value, String> {
    let (path, text) = run_log_store::export_run_log(&run_id)?;
    Ok(json!({ "path": path.to_string_lossy(), "text": text }))
}

pub fn cancel_run(service: &Arc<ModelStoreService>, run_id: String) -> Result<Value, String> {
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
    let runs = match card_id {
        Some(id) => service.active.run_for_card(&id).into_iter().collect(),
        None => service.active.active_runs(),
    };
    Ok(json!({ "runs": runs }))
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
pub fn storage_settings() -> Result<Value, String> {
    let settings = config::load_settings();
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
) -> Result<Value, String> {
    // 先读再改再整体写回：配置里还有 hf_endpoint / staging_dir 等一堆字段，
    // 只写自己关心的那个会把其余的全清掉。
    let mut settings = config::load_settings();
    if let Some(dir) = models_dir {
        let dir = dir.trim().to_string();
        if dir.is_empty() {
            return Err("权重目录不能为空".into());
        }
        // 相对路径在这里是陷阱：卡片脚本各自在不同的工作目录下跑，"./models"
        // 是相对谁说不清楚，真出问题时现象是"模型下到了奇怪的地方"。
        if !std::path::Path::new(&dir).is_absolute() {
            return Err(format!("权重目录要填绝对路径，收到的是「{dir}」"));
        }
        // 建不出来就不写进配置。否则要等到下次下载才在脚本里炸，那时的报错
        // （某个 mkdir 失败）离真正的原因——设置页填错了路径——已经很远了。
        std::fs::create_dir_all(&dir).map_err(|e| format!("无法创建权重目录「{dir}」：{e}"))?;
        settings.models_dir = dir;
    }
    if let Some(dirs) = import_scan_dirs {
        // 滤掉空白项：界面上多敲一次回车就会留一行空的，而空字符串进了搜索
        // 根列表只会白占一行。
        settings.import_scan_dirs = dirs
            .into_iter()
            .map(|d| d.trim().to_string())
            .filter(|d| !d.is_empty())
            .collect();
    }
    config::save_settings(&settings).map_err(|e| format!("保存设置失败：{e}"))?;
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
    let _card_guard = super::runtime_guard::StartupGuard::for_card(&card_id)?;
    service.active.try_begin(&card_id, OP_FORCE_REMOVE, None)?;
    let result = ModelManager::new(None, None)
        .force_remove_record(&card_id)
        .map_err(|e| e.to_string())
        .and_then(|r| serde_json::to_value(r).map_err(|e| e.to_string()));
    service.active.finish(&card_id);
    result
}

#[cfg(test)]
mod tests {
    use super::*;

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
        let mut ring = LineRing::new();
        for index in 0..(MAX_MEMORY_LINES + 10) {
            ring.push(&format!("line-{index}"));
        }
        // 总行数照实数，不因为丢了行就缩回去
        assert_eq!(ring.total(), MAX_MEMORY_LINES + 10);
        assert_eq!(ring.base, 10);
        // 游标落在还留着的区间：从那一行开始给
        let tail = ring.slice_from(MAX_MEMORY_LINES + 8);
        assert_eq!(
            tail,
            vec![
                format!("line-{}", MAX_MEMORY_LINES + 8),
                format!("line-{}", MAX_MEMORY_LINES + 9)
            ]
        );
        // 游标落在已经丢掉的区间：从现存最老的那一行开始给，**不是**从 0
        let from_stale = ring.slice_from(0);
        assert_eq!(from_stale.len(), MAX_MEMORY_LINES);
        assert_eq!(from_stale[0], "line-10");
        // 游标已经追到头：给空，别让轮询以为有新行
        assert!(ring.slice_from(ring.total()).is_empty());
    }

    #[test]
    fn prompt_markers_do_not_shadow_each_other() {
        assert!(is_prompt_line("LMM_PROMPT=text|要点什么"));
        assert!(is_prompt_line("  LMM_PROMPT_FORM={}  "));
        assert!(!is_prompt_line("LMM_PROMPTX=1"));
        assert!(!is_prompt_line("正在下载 …"));
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
        let body = SERVICE_SOURCE
            .split("pub async fn run_log(")
            .nth(1)
            .expect("run_log 没了？这条测试要跟着改");
        let registered = body
            .find("notified.as_mut().enable()")
            .expect("run_log 里找不到 enable()：等待者没有被提前登记，会丢唤醒");
        let snapshot = body
            .find("log.snapshot(from)")
            .expect("run_log 里找不到 snapshot 调用");
        assert!(
            registered < snapshot,
            "登记等待者必须在取快照之前。现在反了——worker 在这两步之间 push 的行             会丢唤醒，本次长轮询要挂满 25 秒，而这条路径上跑的是交互提示。"
        );
    }
}
