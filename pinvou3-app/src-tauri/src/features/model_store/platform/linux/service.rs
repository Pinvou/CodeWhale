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
    /// 脚本有没有声明「失败后保留原生引擎进程」（`LMM_RETAINED_NATIVE_PROCESS=1`）。
    ///
    /// 声明过就**置位到底**：界面的「二次确认停用」入口靠它，而它以前只能从日志行
    /// 里认——回放一裁剪（前端只回放末尾 200 行）标志就丢，任务过期后残存进程占着
    /// 内存却没有任何常驻停用入口。和 `awaiting_prompt` 一样由后端结构化下发。
    retained_native: Mutex<bool>,
    finished_at: Mutex<Option<Instant>>,
    /// 有新行或结束时唤醒正在长轮询的调用
    notify: Arc<tokio::sync::Notify>,
}

impl RunLog {
    fn new(run_id: &str, label: &str, active_run_ids: &[String]) -> Self {
        Self {
            lines: Mutex::new(LineRing::new()),
            disk: RunLogFile::open(run_id, label, active_run_ids),
            docker_progress_notice_emitted: Mutex::new(false),
            outcome: Mutex::new(None),
            cancelled: Mutex::new(false),
            awaiting_prompt: Mutex::new(false),
            retained_native: Mutex::new(false),
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
            retained_native: Mutex::new(false),
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
    pub(super) lan: super::lan_gateway::Gateway,
    registry: CancelRegistry,
    active: Arc<ActiveCardOps>,
    queue: Arc<DownloadQueue>,
    runs: Mutex<HashMap<String, Arc<RunLog>>>,
    startup_cleanup: StartupCleanup,
    startup_updates: super::startup_updates::StartupUpdates,
    models_changed: Mutex<Option<super::super::super::ModelsChangedHandler>>,
}

impl Default for ModelStoreService {
    fn default() -> Self {
        Self::new()
    }
}

impl ModelStoreService {
    pub fn new() -> Self {
        Self {
            lan: super::lan_gateway::Gateway::default(),
            registry: CancelRegistry::default(),
            active: Arc::new(ActiveCardOps::default()),
            queue: Arc::new(DownloadQueue::default()),
            runs: Mutex::new(HashMap::new()),
            startup_cleanup: StartupCleanup::default(),
            startup_updates: Default::default(),
            models_changed: Mutex::new(None),
        }
    }

    pub fn set_models_changed_handler(&self, handler: super::super::super::ModelsChangedHandler) {
        *self.models_changed.lock() = Some(handler);
    }

    fn sync_linked_models(&self, card_id: Option<&str>) -> Result<(), String> {
        let changed = super::linked_models::sync(card_id)?;
        self.notify_models_changed(&changed);
        Ok(())
    }

    fn notify_models_changed(&self, changed: &[String]) {
        if !changed.is_empty() {
            let handler = self.models_changed.lock().clone();
            if let Some(handler) = handler {
                handler(changed);
            }
        }
    }

    fn refresh_enabled_model(&self, card_id: &str) -> Result<(), String> {
        self.sync_linked_models(Some(card_id))?;
        self.notify_models_changed(&super::linked_models::current_linked_ids(card_id));
        Ok(())
    }

    fn new_run(&self, run_id: &str, label: &str) -> Arc<RunLog> {
        self.sweep_old_runs();
        // 新建日志前把「现有活跃 + 自己」交给轮转：正在跑的运行日志不能被
        // prune 删掉（等输入的静默期里 mtime 会变旧，恰好落在删除线上）。
        let mut active: Vec<String> = self.runs.lock().keys().cloned().collect();
        active.push(run_id.to_string());
        let log = Arc::new(RunLog::new(run_id, label, &active));
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
    // run_id 要在占坑之前先发出来：互斥登记里存着它，界面切走再回来时正是按
    // 卡片把它要回去接着读日志的（见 `ActiveCardOps::run_for_card`）。
    // 但登记表之外的东西一律等占坑成功再建——占不到坑就直接返回，不能留下一条
    // 没人会去读、也没人会去收的运行记录。
    let run_id = new_run_id();
    // All lifecycle actions share the lock with package replacement, including
    // another app process. Closing the fd releases it after panic/cancellation.
    let operation_lock = card_operation_lock(card_id)?;
    // PR 侧的按卡保护锁（哈希文件名，任意 card_id 可用）：与上面的
    // operation-lock 互为补充，混跑新旧版本的应用窗口时两把都能对上。
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

pub async fn library(service: Arc<ModelStoreService>) -> Result<Value, String> {
    tokio::task::spawn_blocking(move || {
        let mgr = ModelManager::new(None, None);
        // 先按实际容器状态纠正一次 running：设备可能是重启之后才起的应用，
        // 库里的 running 标记这时候是过期的。
        let corrected = mgr.reconcile_running_status();
        let link_sync_error = service.sync_linked_models(None).err();
        let rows = db::list_store_models_light().map_err(|e| e.to_string())?;
        Ok(json!({
            "models": library_payload(rows),
            "corrected": corrected,
            "link_sync_error": link_sync_error,
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
                        // 不说"引擎已就绪"——这条只是排队位的播报，引擎检查/安装
                        // 在轮到之后才做（.48 真机反馈：排队时说就绪、轮到后装
                        // 引擎失败，前后矛盾让用户以为是误判）。
                        log_gate.push(&format!(
                            "已进入下载队列（前面还有 {ahead} 个）\
                         ——可以放着不管，轮到它会自动检查引擎并开始下载"
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
    confirmed: bool,
) -> Result<Value, String> {
    if !confirmed {
        return Err("更新卡片脚本需要明确确认；不会下载或删除模型权重。".into());
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
                // 「这次运行声明过保留原生进程吗」。结构化下发而不是让前端从日志
                // 行里认：回放一裁剪，行可以丢，声明不能丢——丢了它，残存进程就
                // 没有常驻的停用入口了。
                "retained_native_process": *log.retained_native.lock(),
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
