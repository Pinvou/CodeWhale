//! 跨 command 共享的运行时状态。

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use parking_lot::Mutex;

use super::script_runner::{PromptSlot, RunControl};

/// 正在进行的长操作（下载/部署/启用/停用/卸载）的控制通道登记表。
///
/// 前端发起操作时带一个自己生成的 run_id：
/// - 取消时用同一个 run_id 调 `cancel_operation`，后端把原子标记置位，
///   script_runner 的轮询循环看到后走取消流程（对应 Python 版的
///   threading.Event）；
/// - 脚本打印 `LMM_PROMPT=` 请求输入时，界面弹框、用户答完调
///   `answer_script_prompt`，答案写进同一个 run_id 的应答槽，执行循环取走后
///   写回脚本的 stdin。
///
/// 两者共用一张表：它们的生命周期完全一致，都是"一次运行"的附属物，分成两张
/// 表只会多一处忘记销号的地方。
#[derive(Default)]
pub struct CancelRegistry(Mutex<HashMap<String, RunControl>>);

impl CancelRegistry {
    pub fn register(&self, run_id: &str) -> RunControl {
        let control = RunControl {
            cancel: Some(Arc::new(AtomicBool::new(false))),
            prompt: Some(Arc::new(PromptSlot::default())),
        };
        self.0.lock().insert(run_id.to_string(), control.clone());
        control
    }

    pub fn cancel(&self, run_id: &str) {
        if let Some(c) = self.0.lock().get(run_id) {
            if let Some(f) = &c.cancel {
                f.store(true, Ordering::SeqCst);
            }
            // 正卡在等输入上的运行，光置取消标记还不够：执行循环在等答案，
            // 也要把输入框那边一起中断掉，否则要等满 10 分钟才反应。
            if let Some(p) = &c.prompt {
                p.abort();
            }
        }
    }

    /// 用户答完了脚本请求的那次输入。`None` 表示用户点了取消。
    ///
    /// 在输入框上点取消，和在任务卡片上点取消，对用户是同一件事——都是"我不做了"。
    /// 所以这里必须**同时**置取消标记，不能只 abort 输入框：
    /// 只 abort 的话执行循环确实会停，但 `control.cancelled()` 仍然是 false，
    /// 这次运行会被记成普通失败。界面据此把它显示成红色报错并自动展开日志，
    /// 等于告诉用户"它炸了"——而实际上是他自己停的。
    /// 取消和失败必须分得开，这是这条链路上不能破的规矩。
    pub fn answer_prompt(&self, run_id: &str, answer: Option<String>) {
        if let Some(c) = self.0.lock().get(run_id) {
            match answer {
                Some(text) => {
                    if let Some(p) = &c.prompt {
                        p.answer(text);
                    }
                }
                None => {
                    // 注意不能在这里调 self.cancel()：锁不可重入，会当场死锁。
                    // 把两件事就地做掉，语义跟 cancel() 保持一致。
                    if let Some(f) = &c.cancel {
                        f.store(true, Ordering::SeqCst);
                    }
                    if let Some(p) = &c.prompt {
                        p.abort();
                    }
                }
            }
        }
    }

    pub fn remove(&self, run_id: &str) {
        self.0.lock().remove(run_id);
    }
}

/// 每张卡片同时只允许一个生命周期操作（下载/部署/启用/停用/卸载）。
///
/// 界面上按钮会在操作期间禁用，但那只是第一道防线：按钮状态是前端状态，连点、
/// 事件竞态、甚至第二个应用实例都可能绕过去。真机上两次都栽在这里——
///
/// - 同一张卡跑两份 `hf download`，不是「快一倍」而是两个进程抢同一批文件的锁
///   互相拖累，实测速度从 2.2MB/s 掉到 569KB/s；
/// - 启用一个 67GB 的模型要十几分钟，这期间 `running` 还没置位、按钮一直可点，
///   点几下就真的去起几个容器。
///
/// 这些操作之间也不该并发：正在下载时点部署、正在启用时点停用，都会让脚本
/// 互相踩。所以一张卡同一时刻只放行一个，并把正在进行的操作名回给用户。
#[derive(Default)]
pub struct ActiveCardOps(Mutex<HashMap<String, &'static str>>);

impl ActiveCardOps {
    /// 占坑。这张卡已经有操作在跑时返回 Err，内容是正在进行的那个操作名。
    pub fn try_begin(&self, card_id: &str, op: &'static str) -> Result<(), String> {
        let mut m = self.0.lock();
        if let Some(running) = m.get(card_id) {
            return Err(format!("该模型正在{running}，请等当前操作结束后再试"));
        }
        m.insert(card_id.to_string(), op);
        Ok(())
    }

    pub fn finish(&self, card_id: &str) {
        self.0.lock().remove(card_id);
    }

    /// 现在有没有下载在跑。清理下载缓存前要确认——别把正在写的临时块删了。
    /// 只看下载：部署/启用这些不碰 .incomplete，没必要一并挡住。
    pub fn any_download_running(&self) -> bool {
        self.0.lock().values().any(|op| *op == OP_DOWNLOAD)
    }
}

pub const OP_DOWNLOAD: &str = "下载";
pub const OP_DEPLOY: &str = "部署";
pub const OP_ENABLE: &str = "启用";
pub const OP_STOP: &str = "停用";
pub const OP_UNINSTALL: &str = "卸载";

/// 下载排队。
///
/// 为什么要排队而不是放开并行：多个下载抢的是同一条带宽，并行不会更快。真机
/// 上实测同时下两份时单个速度从 2.2MB/s 掉到 569KB/s，总吞吐反而更低。所以
/// 默认只放一个跑，其余排队（数量可在设置里调到最多 3）。
///
/// 关键设计：**排的只是「下模型权重」这一步**。装引擎那步要 pkexec 弹管理员
/// 密码框，必须趁用户还在电脑前的时候做掉；等真轮到它下载时用户可能早走开了，
/// 那时候弹密码框就是干等到超时。所以 download_from_store 把装引擎放在排队
/// 之前、下权重放在排队之后。
///
/// 用取号的方式保证先来后到：单纯轮询「有没有空位」的话，谁抢到算谁的，
/// 后点的可能插到先点的前面。
#[derive(Default)]
pub struct DownloadQueue(Mutex<QueueState>);

#[derive(Default)]
struct QueueState {
    next_ticket: u64,
    /// 排队中的号，按先来后到
    waiting: std::collections::VecDeque<(u64, String)>,
    /// 正在下载的 run_id
    running: HashSet<String>,
}

impl DownloadQueue {
    /// 取号排队。返回的号用于后续 try_start / position。
    pub fn enqueue(&self, run_id: &str) -> u64 {
        let mut st = self.0.lock();
        st.next_ticket += 1;
        let ticket = st.next_ticket;
        st.waiting.push_back((ticket, run_id.to_string()));
        ticket
    }

    /// 轮到这个号了吗？轮到就占位并返回 true。
    ///
    /// 必须同时满足「有空位」和「自己是队首」——只判空位的话，后来的可能抢在
    /// 先来的前面。
    pub fn try_start(&self, ticket: u64, limit: usize) -> bool {
        let mut st = self.0.lock();
        if st.running.len() >= limit {
            return false;
        }
        if st.waiting.front().map(|(t, _)| *t) != Some(ticket) {
            return false;
        }
        let (_, run_id) = st.waiting.pop_front().expect("刚判断过队首存在");
        st.running.insert(run_id);
        true
    }

    /// 前面还有几个在等（用于给用户显示排到第几位）。
    pub fn position(&self, ticket: u64) -> usize {
        let st = self.0.lock();
        st.waiting
            .iter()
            .position(|(t, _)| *t == ticket)
            .unwrap_or(0)
    }

    /// 结束（下完/失败/取消都要调）。没占到位的也能安全调用。
    pub fn release(&self, run_id: &str) {
        let mut st = self.0.lock();
        st.running.remove(run_id);
        st.waiting.retain(|(_, r)| r != run_id);
    }
}

/// 启动时清理残留脚本进程的结果，先攒在这里，等前端界面起来后取走并提示用户。
/// 用 take 语义（取一次就没了），避免用户切来切去反复弹同一条提示。
#[derive(Default)]
pub struct StartupCleanup(Mutex<Option<super::orphans::OrphanCleanup>>);

impl StartupCleanup {
    pub fn set(&self, report: super::orphans::OrphanCleanup) {
        *self.0.lock() = Some(report);
    }

    pub fn take(&self) -> Option<super::orphans::OrphanCleanup> {
        self.0.lock().take()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn one_lifecycle_op_per_card() {
        let ops = ActiveCardOps::default();
        assert!(ops.try_begin("card-a", OP_ENABLE).is_ok());

        // 同一张卡再来任何操作都要被拦下，并且要说清楚正在干什么
        let err = ops.try_begin("card-a", OP_ENABLE).unwrap_err();
        assert!(err.contains("启用"), "错误信息要点明正在进行的操作：{err}");
        assert!(ops.try_begin("card-a", OP_STOP).is_err(), "启用期间不该放行停用");

        // 别的卡不受影响
        assert!(ops.try_begin("card-b", OP_DOWNLOAD).is_ok());

        ops.finish("card-a");
        assert!(ops.try_begin("card-a", OP_DEPLOY).is_ok(), "结束后应能再次占坑");
    }

    #[test]
    fn only_downloads_block_cache_cleanup() {
        let ops = ActiveCardOps::default();
        assert!(!ops.any_download_running());

        // 部署/启用不碰 .incomplete，不该挡住清理下载缓存
        ops.try_begin("card-a", OP_DEPLOY).unwrap();
        assert!(!ops.any_download_running());

        ops.try_begin("card-b", OP_DOWNLOAD).unwrap();
        assert!(ops.any_download_running());

        ops.finish("card-b");
        assert!(!ops.any_download_running());
    }

    #[test]
    fn queue_is_first_come_first_served() {
        let q = DownloadQueue::default();
        let a = q.enqueue("run-a");
        let b = q.enqueue("run-b");
        let c = q.enqueue("run-c");

        // 限额 1：只有队首能开跑
        assert!(q.try_start(a, 1));
        assert!(!q.try_start(b, 1), "已经有一个在跑，第二个必须等");
        assert!(!q.try_start(c, 1));
        assert_eq!(q.position(b), 0, "b 排在等待队列第一个");
        assert_eq!(q.position(c), 1);

        // a 下完了，轮到 b，而不是谁抢到算谁的
        q.release("run-a");
        assert!(!q.try_start(c, 1), "c 比 b 后来，不能插队");
        assert!(q.try_start(b, 1));

        // 限额放宽到 2 时 c 才能一起跑
        assert!(q.try_start(c, 2));
    }

    #[test]
    fn cancelling_while_queued_frees_the_slot_for_the_next_one() {
        let q = DownloadQueue::default();
        let a = q.enqueue("run-a");
        let b = q.enqueue("run-b");
        let c = q.enqueue("run-c");
        assert!(q.try_start(a, 1));

        // 用户在排队期间点了取消：b 出队，不能把后面的人堵死
        q.release("run-b");
        q.release("run-a");
        assert!(!q.try_start(b, 1), "已取消的号不该还能开跑");
        assert!(q.try_start(c, 1), "取消的那个不能挡住后面的");
    }

    /// 在脚本的输入框上点取消，和在任务卡片上点取消，必须是同一件事。
    ///
    /// 只 abort 输入框而不置取消标记的话，这次运行会被记成普通失败，界面显示成
    /// 红色报错并自动展开日志——等于告诉用户"它炸了"，而实际上是他自己停的。
    #[test]
    fn aborting_a_prompt_marks_the_run_cancelled_not_failed() {
        let registry = CancelRegistry::default();
        let control = registry.register("run-x");
        assert!(!control.cancelled(), "刚登记时不该是已取消");

        registry.answer_prompt("run-x", None);
        assert!(
            control.cancelled(),
            "在输入框上取消，必须跟卡片上取消一样置上取消标记，否则会被当成失败"
        );
    }

    /// 正常作答不能被误当成取消。
    #[test]
    fn answering_a_prompt_does_not_cancel_the_run() {
        let registry = CancelRegistry::default();
        let control = registry.register("run-y");
        registry.answer_prompt("run-y", Some("yes".to_string()));
        assert!(!control.cancelled(), "答了话怎么能算取消");
    }
}
