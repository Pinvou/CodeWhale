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
use super::run_state::{
    ActiveCardOps, CancelRegistry, CardOp, DownloadQueue, OP_DEPLOY, OP_DOWNLOAD, OP_ENABLE,
    OP_FORCE_REMOVE, OP_STOP, OP_UNINSTALL, StartupCleanup,
};
use super::{config, db, disk, import_scan, script_runner};

/// 运行记录保留多久。一次下载的日志能有几万行，不清会一直涨。
const RUN_RETENTION: Duration = Duration::from_secs(600);
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

// ---------------------------------------------------------------- 运行记录

/// 一次长操作的输出与结局。
///
/// 行存在 `Vec` 里而不是只往外推：调用方可能晚到（界面切走再切回来、应用重启），
/// 没有回放就会丢掉前半段日志，而前半段往往正是出错的地方。
struct RunLog {
    lines: Mutex<Vec<String>>,
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
    fn new() -> Self {
        Self {
            lines: Mutex::new(Vec::new()),
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
        self.lines.lock().push(line.to_string());
        self.notify.notify_waiters();
    }

    /// 界面答完了。不等脚本打出下一行就落下标志：用户完全可能答完立刻切走，
    /// 那一瞬间再回来的话，回放会把同一条提示又弹一次。
    fn answered(&self) {
        *self.awaiting_prompt.lock() = false;
    }

    fn finish(&self, outcome: Result<Value, String>, cancelled: bool) {
        *self.cancelled.lock() = cancelled;
        *self.awaiting_prompt.lock() = false;
        *self.outcome.lock() = Some(outcome);
        *self.finished_at.lock() = Some(Instant::now());
        self.notify.notify_waiters();
    }

    fn snapshot(&self, from: usize) -> (Vec<String>, usize, Option<Result<Value, String>>) {
        let lines = self.lines.lock();
        let total = lines.len();
        let slice = if from < total {
            lines[from..].to_vec()
        } else {
            Vec::new()
        };
        (slice, total, self.outcome.lock().clone())
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

    fn new_run(&self, run_id: &str) -> Arc<RunLog> {
        self.sweep_old_runs();
        let log = Arc::new(RunLog::new());
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

fn manager(test_mode: bool) -> ModelManager {
    if test_mode {
        ModelManager::new_test_store()
    } else {
        ModelManager::new(None, None)
    }
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
    let flags: Vec<bool> = models.iter().map(needs_redeploy).collect();
    let mut value = strip_bulky_fields(models);
    if let Some(items) = value.as_array_mut() {
        for (item, needs) in items.iter_mut().zip(flags) {
            if let Some(obj) = item.as_object_mut() {
                obj.insert("needs_redeploy".to_string(), Value::Bool(needs));
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
    // 卡片级互斥：同一张卡不许被两处同时操作。
    service.active.try_begin(card_id, op, Some(&run_id))?;

    let log = service.new_run(&run_id);
    let control = service.registry.register(&run_id);

    let svc = Arc::clone(service);
    let card = card_id.to_string();
    let rid = run_id.clone();
    let log_for_thread = Arc::clone(&log);
    // 留一份给结束后回查：control 本身要 move 进 job
    let control_probe = control.clone();
    let rid_for_job = run_id.clone();
    std::thread::spawn(move || {
        let mut guard = OpCleanup {
            registry_owner: svc,
            card,
            run_id: rid,
            log: Arc::clone(&log_for_thread),
            control: control_probe,
            outcome: None,
        };
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
    });

    Ok(json!({ "run_id": run_id }))
}

// ---------------------------------------------------------------- 只读查询

pub async fn list_cards(test_mode: bool) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || {
        manager(test_mode)
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

// ---------------------------------------------------------------- 长操作

pub fn start_download(
    service: &Arc<ModelStoreService>,
    card_id: String,
    test_mode: bool,
) -> Result<Value, String> {
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
            manager(test_mode)
                .download_from_store(&card_for_job, &mut cb, control, Some(&mut gate))
                .map(|r| serde_json::to_value(r).unwrap_or(Value::Null))
                .map_err(|e| e.to_string())
        },
    )
}

pub fn start_deploy(
    service: &Arc<ModelStoreService>,
    card_id: String,
    profile_id: Option<String>,
) -> Result<Value, String> {
    let card = card_id.clone();
    spawn_op(
        service,
        &card_id,
        OP_DEPLOY,
        move |log, control, _run_id| {
            let log_cb = Arc::clone(log);
            let mut cb = move |line: &str| log_cb.push(line);
            ModelManager::new(None, None)
                .deploy(&card, profile_id.as_deref(), &mut cb, control)
                .map(|_| Value::Null)
                .map_err(|e| e.to_string())
        },
    )
}

/// 启用 / 停用 / 卸载三个动作形状一样，只差调哪个方法，合并成一个入口。
///
/// `purge_weights` 只对卸载有意义，而且**缺省是保留**——不可逆的那一侧绝不能靠
/// "没写"命中。
pub fn start_lifecycle(
    service: &Arc<ModelStoreService>,
    card_id: String,
    action: String,
    purge_weights: bool,
) -> Result<Value, String> {
    let op = match action.as_str() {
        "enable" => OP_ENABLE,
        "stop" => OP_STOP,
        "uninstall" => OP_UNINSTALL,
        other => return Err(format!("未知操作：{other}")),
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
        let notified = log.notify.notified();
        let _ = tokio::time::timeout(deadline.saturating_duration_since(Instant::now()), notified)
            .await;
    }
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

// ---------------------------------------------------------------- 测试商店开关

/// 测试商店口令的 sha256。
///
/// 只存哈希、且校验放在后端：明文写进前端的话，打开开发者工具就看见了。
/// 但要说清楚——**这是防呆，不是防攻击**。拿到 deb 逆向出这个哈希、再爆破一个
/// 简单口令是分分钟的事。它挡的是「客户误点进来」，挡不住「有人存心要开」。
const TEST_STORE_PASSWORD_SHA256: &str =
    "b1462f7b2997b606480c26ab059074104a27bf667f406f9b4d8af38132ba5772";

/// 首尾空白要 trim 掉：从别处复制口令时经常带着换行，那不该被当成输错。
fn test_store_password_matches(input: &str) -> bool {
    use sha2::{Digest, Sha256};
    crate::platform::encoding::hex_lower(&Sha256::digest(input.trim().as_bytes()))
        == TEST_STORE_PASSWORD_SHA256
}

pub fn test_store_state() -> Result<Value, String> {
    Ok(json!({ "enabled": config::load_settings().test_mode_enabled }))
}

/// **口令错也算成功返回**，body 是 `{"ok": false}`。
///
/// 口令不对不是错误：调用方要能区分「口令输错了」和「配置没写进去」——前者提示
/// 用户重输，后者得让用户去看磁盘/权限。都压成 Err 的话这两件事在界面上长得
/// 一模一样。真的写不进配置文件才返回 Err。
pub fn unlock_test_store(password: String) -> Result<Value, String> {
    if !test_store_password_matches(&password) {
        return Ok(json!({ "ok": false }));
    }
    set_test_store_enabled(true)?;
    Ok(json!({ "ok": true }))
}

pub fn disable_test_store() -> Result<Value, String> {
    set_test_store_enabled(false)?;
    Ok(json!({ "enabled": false }))
}

fn set_test_store_enabled(enabled: bool) -> Result<(), String> {
    // 先读再改再整体写回：配置里还有 store_url / models_dir 等一堆字段，
    // 只写自己关心的那个会把其余的全清掉。
    let mut settings = config::load_settings();
    settings.test_mode_enabled = enabled;
    config::save_settings(&settings).map_err(|e| format!("保存设置失败：{e}"))
}

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

/// 启动时把内置（测试商店）卡片的固化脚本刷新成应用当前版本。
///
/// **必须在启动阶段真的被调用**——`refresh_builtin_cards` 的文档一直写着「应用
/// 启动时跑一次」，但在此之前全仓零调用点。它要防的事故在真机上发生过：推了新
/// 应用、脚本改动一个都没生效，卡片跑的还是下载那天固化的旧脚本，白查了一轮。
///
/// 内置卡的脚本正文编译在二进制里，所以应用升级就等于脚本升级，不该还要用户
/// 手动点一次「检查卡片更新」。
///
/// 失败不影响启动：它只碰 `card_source = 'builtin'` 的记录，刷不动就维持原样。
pub fn run_startup_refresh() {
    let refreshed = ModelManager::refresh_builtin_cards();
    if !refreshed.is_empty() {
        eprintln!(
            "[model-store] 启动刷新：{} 张内置卡片的脚本已更新到当前应用版本（{}）",
            refreshed.len(),
            refreshed.join("、")
        );
    }
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
    // 先读再改再整体写回，理由同 set_test_store_enabled
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

    /// 「现在还有没有提示在等人答」只有后端知道，而这件事必须由后端说了算。
    ///
    /// 界面是从日志行里解析提示的，而日志可以回放（切走再回来会从头读一遍）。
    /// 答过的提示被回放时再弹一次的后果不是多点一下：用户填的答案会写进一个没人
    /// 来取的应答槽，等脚本下一次真的提问时被这份陈答案直接顶掉——双机部署里
    /// 那就是拿着上一轮的 IP 去连从机。
    #[test]
    fn a_prompt_stops_awaiting_once_anything_else_comes_out() {
        let log = RunLog::new();
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
        let log = RunLog::new();
        log.push("LMM_PROMPT=password|输入本机密码");
        log.answered();
        assert!(!*log.awaiting_prompt.lock());
    }

    /// 跑完了就没有什么可答的了。留着这个标志，界面接回一条已结束的运行时会弹出
    /// 一个永远送不出去的输入框。
    #[test]
    fn finishing_clears_awaiting() {
        let log = RunLog::new();
        log.push("LMM_PROMPT=confirm|继续吗");
        log.finish(Err("脚本非零退出".to_string()), false);
        assert!(!*log.awaiting_prompt.lock());
    }

    #[test]
    fn prompt_markers_do_not_shadow_each_other() {
        assert!(is_prompt_line("LMM_PROMPT=text|要点什么"));
        assert!(is_prompt_line("  LMM_PROMPT_FORM={}  "));
        assert!(!is_prompt_line("LMM_PROMPTX=1"));
        assert!(!is_prompt_line("正在下载 …"));
    }
}
