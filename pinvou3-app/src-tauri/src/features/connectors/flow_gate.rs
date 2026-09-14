//! 连接器 SSO 登录流程的单飞门闩（EIP / 知道共用）。
//!
//! 背景：SSO 网关对同一设备的 `auth login` 有限流（短时间内第二次返回 429
//! 「当前时间段内操作过频繁」）。用户「关闭连接弹窗 → 立刻重开」是高频操作，
//! 若每次重开都新起一条登录流程并再发一次 `auth login`，必然撞限流。
//!
//! 门闩把连接编排收敛为三态：
//! - `Idle`：无进行中流程，`begin` 返回 [`Begin::Start`]，调用方启动新流程；
//! - `Running`：流程在轮询且未被取消，`begin` 返回 [`Begin::Reuse`] 并附缓存的
//!   SSO 载荷，调用方重放给前端即可——**不再发第二次 `auth login`**；
//! - `Cancelling`：已被请求取消但流程尚未退出（一拍 poll+sleep），`begin` 返回
//!   [`Begin::Busy`]，调用方应等待 [`SsoFlow::release`] 后重试。
//!
//! 只有流程自身在退出时归还门闩；取消只是把 `Running` 转成 `Cancelling`，杜绝
//! 「复用一个将死流程」的竞态。归还通过 [`FlowGuard`]（Drop）完成——流程体的
//! `JoinHandle` 在调用方被丢弃，没有任何外部力量能替它收尾，靠流程自己逐个
//! `return` 前调 `finish()` 会漏（panic / 新增退出路径），一旦漏掉门闩就永久压在
//! `Running`，之后每次连接都重放一张没人轮询的死二维码。
//!
//! 每个门闩持有者带一个 `epoch`（代次）：
//! - 抢占：`Running`/`Cancelling` 超过 [`STALL_LIMIT`] 无心跳（子进程僵死，
//!   `cmd.output()` 可无限阻塞）时 `begin` 直接回收门闩交给新流程自愈；
//! - 隔离：被抢占的僵尸流程醒来后 epoch 已过期，其 `release`/`arm_payload`/
//!   心跳全部失效，不会污染新持有者的状态。
//!
//! 另提供进程级 [`throttle_sso_login`]：EIP 与知道的 login 端点同源（SSO_NEXT，
//! 同一网关按设备限流），跨连接器快速先后发起也会 429，单飞门闩管不到这种
//! 场景，故任意两次 SSO login 请求之间强制最小间隔。

use std::sync::{LazyLock, Mutex};
use std::time::{Duration, Instant};

/// `begin` 的判定结果。
pub(crate) enum Begin {
    /// 无进行中流程：调用方携 `epoch` 启动新流程。`epoch` 是本次持有权的凭证，
    /// 必须一路传给流程体与 [`FlowGuard`]，否则流程无法证明自己还持有门闩。
    Start { epoch: u64 },
    /// 已有未取消的流程在跑：重放缓存的 SSO 载荷（`None` = 流程刚启动、URL 还没
    /// 拿到，其在途的 `eip:sso`/`zhidao:sso` 事件随后会送达，无须补发）。
    Reuse {
        payload: Option<(String, Option<String>)>,
    },
    /// 旧流程已被取消但还没退出：稍后重试。
    Busy,
}

#[derive(Default)]
pub(crate) struct SsoFlow {
    state: Mutex<State>,
}

#[derive(Default)]
struct State {
    phase: Phase,
    /// 当前门闩持有者的代次——`Start` 时自增。流程只在 `epoch` 相符时才能写入
    /// 载荷 / 刷心跳 / 归还门闩，其余一律视为已被抢占的僵尸流程。
    epoch: u64,
    /// 进行中流程的 SSO 载荷（url, qr_data_url），供 Reuse 重放。
    payload: Option<(String, Option<String>)>,
    /// 最近一次心跳。流程每发起一次**可能长时间阻塞**的子进程调用前刷新，
    /// 用于识别"卡死在子进程里"的僵尸流程。
    last_progress: Option<Instant>,
    /// 取消代次：每次 [`SsoFlow::request_cancel`] 自增。等待门闩归还的调用方记下
    /// 进入等待时的值，若中途变化说明用户点过取消，它必须放弃起步——否则"取消"
    /// 会被随后的自动起步吃掉（旧流程归还门闩后等待者照常 spawn 新流程，把用户
    /// 刚取消的弹窗重新弹出来）。
    cancel_seq: u64,
}

/// 等待门闩归还期间的"取消监票"。见 [`SsoFlow::cancel_ticket`]。
#[derive(Clone, Copy, PartialEq, Debug)]
pub(crate) struct CancelTicket(u64);

#[derive(Default, PartialEq, Debug)]
enum Phase {
    #[default]
    Idle,
    Running,
    Cancelling,
}

impl State {
    /// 门闩被非 Idle 流程占着且久无心跳 = 子进程僵死。判据是 [`STALL_LIMIT`]，
    /// 而流程在每个阻塞调用前都刷心跳，所以该值只需覆盖**单次**调用的最坏耗时。
    fn is_stalled(&self) -> bool {
        self.phase != Phase::Idle
            && self
                .last_progress
                .is_some_and(|t| t.elapsed() > STALL_LIMIT)
    }
}

/// 判定流程"卡死"的无心跳上限。流程在每个阻塞型 CLI 调用前刷心跳，故只需覆盖
/// 单次调用的最坏耗时：HTTP 客户端超时 60s（见 linux-src/main.go `httpClient`），
/// `auth poll` 自带 30s 轮询窗口、末次请求可能再耗 60s ≈ 90s，取 2 倍留余量。
const STALL_LIMIT: Duration = Duration::from_secs(180);

/// 调用方等待旧流程归还门闩的上限。必须**大于** [`STALL_LIMIT`]，好让"卡死"
/// 由抢占自愈（`begin` 直接返回 `Start`）而不是把用户推回重试提示；同时也要
/// 覆盖最坏的一次**合法**取消节拍（3s sleep + poll 90s + status 60s ≈ 153s）。
pub(crate) const CANCEL_WAIT_LIMIT: Duration = Duration::from_secs(195);

/// 流程持有的门闩守卫。**在流程体最外层作用域创建**，无论正常返回、提前
/// `return` 还是 panic（unwind）都会在 Drop 里归还门闩——这是"流程死了门闩
/// 不落空"的唯一保证。
pub(crate) struct FlowGuard<'a> {
    flow: &'a SsoFlow,
    epoch: u64,
}

impl<'a> FlowGuard<'a> {
    pub(crate) fn new(flow: &'a SsoFlow, epoch: u64) -> Self {
        Self { flow, epoch }
    }
}

impl Drop for FlowGuard<'_> {
    fn drop(&mut self) {
        self.flow.release(self.epoch);
    }
}

impl SsoFlow {
    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        // 中毒时取回数据：状态机字段小且幂等，宁可降级也不让命令 panic。
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// 连接入口裁决。见模块注释的三态语义。
    pub(crate) fn begin(&self) -> Begin {
        let mut s = self.lock();
        if s.is_stalled() {
            // 自愈：子进程僵死时流程内的 300s 总超时到不了，门闩会永久压在
            // Running。直接回收转交新流程；旧流程 epoch 随即过期，它即使醒来
            // 也无法再写状态、发事件。
            log::warn!(
                "[flow_gate] 抢占无心跳流程 phase={:?} epoch={} 已 {:?} 无进展",
                s.phase,
                s.epoch,
                s.last_progress.map(|t| t.elapsed())
            );
            s.phase = Phase::Idle;
            s.payload = None;
        }
        match s.phase {
            Phase::Idle => {
                s.epoch = s.epoch.wrapping_add(1);
                s.phase = Phase::Running;
                s.payload = None;
                s.last_progress = Some(Instant::now());
                Begin::Start { epoch: s.epoch }
            }
            Phase::Running => Begin::Reuse {
                payload: s.payload.clone(),
            },
            Phase::Cancelling => Begin::Busy,
        }
    }

    /// 流程每次发起可能长时间阻塞的子进程调用**前**调用：刷新心跳，并回答
    /// "我还该继续跑吗"。
    ///
    /// 返回 `false` = 已被取消（`Cancelling`，下一拍自停）或已被抢占（epoch
    /// 过期，须静默退出）——两种情况流程都应立刻停止且不再发任何事件。
    pub(crate) fn is_active(&self, epoch: u64) -> bool {
        let mut s = self.lock();
        if s.phase != Phase::Running || s.epoch != epoch {
            return false;
        }
        s.last_progress = Some(Instant::now());
        true
    }

    /// 流程拿到 SSO 地址后缓存载荷，供后续 Reuse 重放。已被抢占则丢弃。
    pub(crate) fn arm_payload(&self, epoch: u64, url: String, qr: Option<String>) {
        let mut s = self.lock();
        if s.phase == Phase::Running && s.epoch == epoch {
            s.payload = Some((url, qr));
        }
    }

    /// 等待门闩归还前取一张取消监票，等待循环每拍用 [`SsoFlow::cancelled_since`]
    /// 核对。等待期间用户点"取消"时，旧流程可能已经是 `Cancelling`（再取消是
    /// no-op）、新流程又还没起步（无人可取消），只有这张票能记下这个意图。
    pub(crate) fn cancel_ticket(&self) -> CancelTicket {
        CancelTicket(self.lock().cancel_seq)
    }

    /// 自取得 `ticket` 以来是否发生过取消请求。
    /// 返回 true = 等待者应放弃本次起步（静默，不要发任何事件）。
    pub(crate) fn cancelled_since(&self, ticket: CancelTicket) -> bool {
        self.lock().cancel_seq != ticket.0
    }

    /// 请求取消：Running → Cancelling，并翻新取消代次。
    ///
    /// 代次**无条件**自增（哪怕相位已是 `Cancelling`、状态机本身没变化）：
    /// 它是"用户按了取消"这个事实的记录，等待门闩的调用方靠它决定要不要放弃
    /// 起步。否则在"旧流程已在取消中 + 用户重连后又取消"这条路径上，这次取消
    /// 会完全落空。
    pub(crate) fn request_cancel(&self) {
        let mut s = self.lock();
        if s.phase == Phase::Running {
            s.phase = Phase::Cancelling;
        }
        s.cancel_seq = s.cancel_seq.wrapping_add(1);
    }

    /// 归还门闩。只由 [`FlowGuard`] 调用——`epoch` 不符说明本流程已被抢占，
    /// 绝不能动当前持有者的状态。
    fn release(&self, epoch: u64) {
        let mut s = self.lock();
        if s.epoch != epoch {
            return;
        }
        s.phase = Phase::Idle;
        s.payload = None;
        s.last_progress = None;
    }
}

/// 两次 SSO login 请求的最小间隔。实测网关限流窗口约 60s（65s 后重试成功），
/// 留 5s 余量取 65s。
const SSO_LOGIN_MIN_INTERVAL: Duration = Duration::from_secs(65);

/// 节流等待的分片粒度：等待期间按此间隔醒来检查一次取消，保证"取消"及时生效。
const THROTTLE_TICK: Duration = Duration::from_millis(250);

/// 进程内最近一次 SSO login 的发起时间（EIP / 知道共用——同源网关，限流按设备算）。
static LAST_SSO_LOGIN: LazyLock<Mutex<Option<Instant>>> = LazyLock::new(|| Mutex::new(None));

/// 发起 SSO login（`auth login` / `login`）前调用：距上次不足间隔则等待补齐，
/// 然后登记本次发起时间。在 spawn_blocking 上下文中调用（阻塞 sleep 安全）。
///
/// - `on_wait`：需要等待时回调一次剩余时长，供调用方给前端出进度提示；
/// - `aborted`：每个分片检查一次，返回 true 则放弃本次登录并回滚预约。
///
/// 返回 `false` = 已被中止，调用方应放弃本次 login（静默退出，不要报错）。
pub(crate) fn throttle_sso_login(
    on_wait: impl FnOnce(Duration),
    mut aborted: impl FnMut() -> bool,
) -> bool {
    throttle_at(
        &LAST_SSO_LOGIN,
        SSO_LOGIN_MIN_INTERVAL,
        on_wait,
        &mut aborted,
    )
}

/// 节流实现：**锁内只做预约、不睡眠**。旧写法持锁 sleep，EIP 与知道并发发起时
/// 第二家会在 mutex 上排队等满第一家的剩余窗口（最坏累计 ~130s）且全程无反馈。
/// 现在两家各自算出"我该等多久"，睡眠在锁外分片进行。
fn throttle_at(
    last: &Mutex<Option<Instant>>,
    min_interval: Duration,
    on_wait: impl FnOnce(Duration),
    aborted: &mut impl FnMut() -> bool,
) -> bool {
    let now = Instant::now();
    // 预约：`*last` 存的是"下一次允许发起的时刻"，故本家排在上一个预约之后。
    // 同时记下被自己覆盖的前值，供中止时回滚。
    let (slot, previous) = {
        let mut guard = last.lock().unwrap_or_else(|e| e.into_inner());
        let previous = *guard;
        let slot = match previous {
            Some(prev) if prev + min_interval > now => prev + min_interval,
            _ => now,
        };
        *guard = Some(slot);
        (slot, previous)
    };

    if slot <= now {
        // 无需等待也要最后看一眼：否则"取消"之后紧跟着的这次 login 仍会真的发出去
        // （流程拿到门闩 → 取消落在 throttle 与 run 之间）。同理被抢占的僵尸流程
        // 也不该再发请求。已预约的槽位要回滚，别让一次没发生的 login 占掉窗口。
        if aborted() {
            rollback(last, slot, previous);
            return false;
        }
        return true;
    }
    if aborted() {
        rollback(last, slot, previous);
        return false;
    }
    on_wait(slot - now);

    loop {
        let remaining = slot.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            // 窗口耗尽的最后一次出队也要监票：取消/抢占落在最后一个分片内时，
            // 这次 login 同样不能真的发出去——与免等待路径同一标准。
            if aborted() {
                rollback(last, slot, previous);
                return false;
            }
            return true;
        }
        if aborted() {
            rollback(last, slot, previous);
            return false;
        }
        std::thread::sleep(THROTTLE_TICK.min(remaining));
    }
}

/// 中止时回滚预约：本次没有真的发请求，不该白白占掉下一个窗口（否则取消后
/// 立刻重连还要再等满一个 65s）。只在没人排在我们后面时才回滚——否则会把
/// 后来者的预约一起抹掉。
fn rollback(last: &Mutex<Option<Instant>>, slot: Instant, previous: Option<Instant>) {
    let mut guard = last.lock().unwrap_or_else(|e| e.into_inner());
    if *guard == Some(slot) {
        *guard = previous;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::panic::{AssertUnwindSafe, catch_unwind};

    /// 起一条流程并拿回它的 epoch，等价于调用方收到 `Begin::Start` 后起步。
    fn start(f: &SsoFlow) -> u64 {
        match f.begin() {
            Begin::Start { epoch } => epoch,
            _ => panic!("idle flow must start"),
        }
    }

    #[test]
    fn second_begin_reuses_running_flow_with_cached_payload() {
        let f = SsoFlow::default();
        let epoch = start(&f);
        f.arm_payload(epoch, "https://sso/x".into(), Some("qr".into()));
        match f.begin() {
            Begin::Reuse { payload } => {
                assert_eq!(payload, Some(("https://sso/x".into(), Some("qr".into()))))
            }
            _ => panic!("running flow must be reused, not restarted"),
        }
    }

    #[test]
    fn begin_before_payload_armed_reuses_without_payload() {
        let f = SsoFlow::default();
        start(&f);
        match f.begin() {
            Begin::Reuse { payload } => assert!(payload.is_none()),
            _ => panic!("running flow must be reused even before payload is armed"),
        }
    }

    #[test]
    fn cancelled_flow_is_busy_until_it_finishes() {
        let f = SsoFlow::default();
        let epoch = start(&f);
        f.request_cancel();
        assert!(
            !f.is_active(epoch),
            "cancelled flow must stop at next check"
        );
        assert!(matches!(f.begin(), Begin::Busy));
        drop(FlowGuard::new(&f, epoch));
        assert!(matches!(f.begin(), Begin::Start { .. }));
    }

    #[test]
    fn cancel_on_idle_is_noop() {
        let f = SsoFlow::default();
        f.request_cancel();
        assert!(matches!(f.begin(), Begin::Start { .. }));
    }

    #[test]
    fn guard_drop_clears_payload() {
        let f = SsoFlow::default();
        let epoch = start(&f);
        f.arm_payload(epoch, "https://sso/x".into(), None);
        drop(FlowGuard::new(&f, epoch));
        let next = start(&f);
        match f.begin() {
            Begin::Reuse { payload } => assert!(
                payload.is_none(),
                "flow started after release must not see a stale payload"
            ),
            _ => panic!("new flow must be running"),
        }
        drop(FlowGuard::new(&f, next));
    }

    /// M1 回归：流程 panic 也不能把门闩永久压在 Running——旧写法靠流程自己逐条
    /// `return` 前调 `finish()`，panic 会绕过全部退出路径。
    #[test]
    fn panicking_flow_still_releases_the_latch() {
        let f = SsoFlow::default();
        let epoch = start(&f);
        let result = catch_unwind(AssertUnwindSafe(|| {
            let _guard = FlowGuard::new(&f, epoch);
            panic!("flow body blew up");
        }));
        assert!(result.is_err(), "the flow body must have panicked");
        assert!(
            matches!(f.begin(), Begin::Start { .. }),
            "latch must be back to Idle so the next connect can start"
        );
    }

    /// M1 回归：子进程僵死时 `begin` 抢占，重开连接拿到新流程而不是一张死二维码。
    #[test]
    fn stalled_running_flow_is_preempted() {
        let f = SsoFlow::default();
        let epoch = start(&f);
        f.arm_payload(epoch, "https://sso/dead".into(), Some("dead-qr".into()));
        // 模拟子进程僵死：心跳停在 STALL_LIMIT 之前。
        f.lock().last_progress = Some(Instant::now() - STALL_LIMIT - Duration::from_secs(1));
        let next = match f.begin() {
            Begin::Start { epoch } => epoch,
            _ => panic!("stalled flow must be preempted, not reused"),
        };
        assert_ne!(next, epoch, "preemption must hand out a fresh epoch");
    }

    /// 抢占后僵尸流程不得污染新持有者：归还、武装载荷、心跳全部失效。
    #[test]
    fn preempted_flow_cannot_touch_the_new_holder() {
        let f = SsoFlow::default();
        let zombie = start(&f);
        f.arm_payload(zombie, "https://sso/zombie".into(), None);
        f.lock().last_progress = Some(Instant::now() - STALL_LIMIT - Duration::from_secs(1));
        let fresh = start(&f);
        assert_ne!(zombie, fresh);

        drop(FlowGuard::new(&f, zombie)); // 僵尸流程终于醒来退出
        assert!(
            matches!(f.begin(), Begin::Reuse { .. }),
            "new holder must survive"
        );
        f.arm_payload(zombie, "https://sso/stale".into(), Some("stale-qr".into()));
        assert!(!f.is_active(zombie), "zombie must not report itself active");
        match f.begin() {
            Begin::Reuse { payload } => assert_ne!(
                payload,
                Some(("https://sso/stale".into(), Some("stale-qr".into()))),
                "zombie must not overwrite the live holder's payload"
            ),
            _ => panic!("new holder must still be running"),
        }
    }

    /// 门闩没被占时不该误抢占（比如刚 `Start` 完、心跳是新的）。
    #[test]
    fn fresh_running_flow_is_not_preempted() {
        let f = SsoFlow::default();
        start(&f);
        assert!(matches!(f.begin(), Begin::Reuse { .. }));
    }

    /// Major 回归：等待门闩期间用户点"取消"必须能被等待者察觉。
    /// 这条路径上旧流程已是 Cancelling（再取消是 no-op）、新流程又还没 begin
    /// （无人可取消），若没有监票，取消会完全落空，等待者随后照常起步、把用户
    /// 刚取消的弹窗重新弹出来。
    #[test]
    fn waiter_can_observe_a_cancel_during_the_wait() {
        let f = SsoFlow::default();
        let epoch = start(&f);
        f.request_cancel(); // 旧流程进入 Cancelling
        let ticket = f.cancel_ticket();
        assert!(!f.cancelled_since(ticket), "刚取的监票不该立刻算作已取消");
        assert!(matches!(f.begin(), Begin::Busy), "此时等待者只能排队");
        f.request_cancel(); // 用户在等待期间点取消：相位没变，但意图必须被记下
        assert!(
            f.cancelled_since(ticket),
            "等待期间的取消必须能被等待者察觉，否则取消会被随后的自动起步吃掉"
        );
        drop(FlowGuard::new(&f, epoch));
        assert!(matches!(f.begin(), Begin::Start { .. }));
    }

    /// 取消后紧接着的放行不能把 login 漏出去：throttle 的免等待路径也要看 abort。
    #[test]
    fn throttle_fast_path_does_not_let_an_aborted_login_through() {
        let last = Mutex::new(None);
        assert!(
            !throttle_at(&last, Duration::from_millis(120), |_| {}, &mut || true),
            "已取消的调用方不该被放行（否则仍会真的发出一次 login）"
        );
        // 且不该占掉窗口：下一个调用方应立即放行。
        let t0 = Instant::now();
        assert!(throttle_at(
            &last,
            Duration::from_millis(120),
            |_| {},
            &mut || false
        ));
        assert!(
            t0.elapsed() < Duration::from_millis(60),
            "未发生的 login 不该占掉节流窗口，等了 {:?}",
            t0.elapsed()
        );
    }

    /// 窗口耗尽的最后一次出队也要监票：取消落在最后一个分片内时，这次 login
    /// 同样不能被放出去，预约也要回滚。
    #[test]
    fn throttle_final_dequeue_does_not_let_an_aborted_login_through() {
        let last = Mutex::new(None);
        assert!(throttle_at(
            &last,
            Duration::from_millis(80),
            |_| {},
            &mut || false
        ));
        let previous = *last.lock().unwrap();
        // abort 在窗口过半后翻真：模拟"取消落在最后一个分片内"。
        let t0 = Instant::now();
        let mut abort_midway = || t0.elapsed() >= Duration::from_millis(40);
        assert!(
            !throttle_at(&last, Duration::from_millis(80), |_| {}, &mut abort_midway),
            "最后一次出队已察觉取消，不该把这次 login 放出去"
        );
        assert_eq!(
            *last.lock().unwrap(),
            previous,
            "没发生的 login 不该占掉节流窗口（预约要回滚）"
        );
    }

    #[test]
    fn throttle_enforces_min_interval_between_logins() {
        let last = Mutex::new(None);
        // 从首次调用之前起计量：第二次调用起步前可能已消耗部分窗口（CI 调度
        // 停顿甚至可能已越过整个窗口——此时立即放行是正确行为），断言的语义
        // 是"不得早于自己的预约槽位放行"，扣掉起步前已消耗的量，另留 2ms
        // 裕量吸收时钟与调度精度；仍能拦住"完全没等"的回归。
        let t0 = Instant::now();
        assert!(throttle_at(
            &last,
            Duration::from_millis(80),
            |_| {},
            &mut || false
        ));
        let entry = Instant::now();
        assert!(throttle_at(
            &last,
            Duration::from_millis(80),
            |_| {},
            &mut || false
        ));
        let waited = entry.elapsed();
        let floor = Duration::from_millis(78).saturating_sub(entry.duration_since(t0));
        assert!(
            waited >= floor,
            "second login must not pass before its reserved slot (waited {waited:?})"
        );
        assert!(last.lock().unwrap().is_some());
    }

    #[test]
    fn throttle_passes_immediately_after_interval() {
        let stale = Mutex::new(Some(Instant::now() - Duration::from_secs(10)));
        let t0 = Instant::now();
        assert!(throttle_at(
            &stale,
            Duration::from_millis(80),
            |_| {},
            &mut || false
        ));
        assert!(
            t0.elapsed() < Duration::from_millis(80),
            "login outside the window must not be delayed"
        );
    }

    /// m4 回归：等待期间不持锁——第一家还在窗口里时，第二家必须能立刻取到锁。
    #[test]
    fn throttle_waits_without_holding_the_lock() {
        let last = Mutex::new(Some(Instant::now())); // 窗口刚过 0s，第二家要等满
        let waiting = std::thread::scope(|scope| {
            let last = &last;
            scope.spawn(move || {
                throttle_at(last, Duration::from_millis(120), |_| {}, &mut || false)
            });
            // 第一家睡在等待里时，主线程必须能马上拿到锁（旧写法会卡满 120ms）。
            let t0 = Instant::now();
            let guard = last.lock().unwrap_or_else(|e| e.into_inner());
            let waited = t0.elapsed();
            drop(guard);
            waited
        });
        assert!(
            waiting < Duration::from_millis(60),
            "lock must be free while another login waits, timed out after {waiting:?}"
        );
    }

    /// m2 回归：等待期间被取消（用户收起弹窗后点取消）必须回滚预约，否则重连
    /// 还要再白等一个完整窗口。
    #[test]
    fn aborted_throttle_wait_rolls_back_the_reservation() {
        let last = Mutex::new(None);
        assert!(throttle_at(
            &last,
            Duration::from_millis(120),
            |_| {},
            &mut || false
        ));
        // 第二家预约成功但立刻被中止。
        assert!(!throttle_at(
            &last,
            Duration::from_millis(120),
            |_| {},
            &mut || true
        ));
        // 回滚后剩下的窗口应约等于第一家的 120ms，而不是再叠加一个。
        let t0 = Instant::now();
        assert!(throttle_at(
            &last,
            Duration::from_millis(120),
            |_| {},
            &mut || false
        ));
        let waited = t0.elapsed();
        assert!(
            waited < Duration::from_millis(180),
            "aborted reservation must not stack another full window, waited {waited:?}"
        );
    }

    #[test]
    fn throttle_reports_the_wait_before_sleeping() {
        let last = Mutex::new(Some(Instant::now()));
        let reported = std::cell::Cell::new(None);
        assert!(throttle_at(
            &last,
            Duration::from_millis(100),
            |wait| reported.set(Some(wait)),
            &mut || false
        ));
        let reported = reported.get().expect("on_wait must fire when delayed");
        assert!(
            reported <= Duration::from_millis(100) && !reported.is_zero(),
            "reported wait must be the remaining window, got {reported:?}"
        );
    }
}
