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
//! - `Cancelling`：已被请求取消但流程尚未退出（一拍 poll+sleep，最坏约 8s），
//!   `begin` 返回 [`Begin::Busy`]，调用方应等待 [`SsoFlow::finish`] 后重试。
//!
//! 只有流程自身在退出时调用 [`SsoFlow::finish`] 归还门闩；取消只是把
//! `Running` 转成 `Cancelling`，杜绝「复用一个将死流程」的竞态。
//!
//! 另提供进程级 [`throttle_sso_login`]：EIP 与知道的 login 端点同源（SSO_NEXT，
//! 同一网关按设备限流），跨连接器快速先后发起也会 429，单飞门闩管不到这种
//! 场景，故任意两次 SSO login 请求之间强制最小间隔。

use std::sync::{LazyLock, Mutex};
use std::time::{Duration, Instant};

/// `begin` 的判定结果。
pub(crate) enum Begin {
    /// 无进行中流程：调用方启动新登录流程。
    Start,
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
    /// 进行中流程的 SSO 载荷（url, qr_data_url），供 Reuse 重放。
    payload: Option<(String, Option<String>)>,
}

#[derive(Default, PartialEq)]
enum Phase {
    #[default]
    Idle,
    Running,
    Cancelling,
}

impl SsoFlow {
    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        // 中毒时取回数据：状态机字段小且幂等，宁可降级也不让命令 panic。
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// 连接入口裁决。见模块注释的三态语义。
    pub(crate) fn begin(&self) -> Begin {
        let mut s = self.lock();
        match s.phase {
            Phase::Idle => {
                s.phase = Phase::Running;
                s.payload = None;
                Begin::Start
            }
            Phase::Running => Begin::Reuse {
                payload: s.payload.clone(),
            },
            Phase::Cancelling => Begin::Busy,
        }
    }

    /// 流程拿到 SSO 地址后缓存载荷，供后续 Reuse 重放。
    pub(crate) fn arm_payload(&self, url: String, qr: Option<String>) {
        let mut s = self.lock();
        if s.phase == Phase::Running {
            s.payload = Some((url, qr));
        }
    }

    /// 请求取消：Running → Cancelling（Idle 下为 no-op）。
    pub(crate) fn request_cancel(&self) {
        let mut s = self.lock();
        if s.phase == Phase::Running {
            s.phase = Phase::Cancelling;
        }
    }

    /// 流程轮询节拍里检查是否该退出。
    pub(crate) fn is_cancelling(&self) -> bool {
        self.lock().phase == Phase::Cancelling
    }

    /// 流程退出（成功/失败/超时/取消）时归还门闩。只能在流程自己的退出路径调用。
    pub(crate) fn finish(&self) {
        let mut s = self.lock();
        s.phase = Phase::Idle;
        s.payload = None;
    }
}

/// 两次 SSO login 请求的最小间隔。实测网关限流窗口约 60s（65s 后重试成功），
/// 留 5s 余量取 65s。
const SSO_LOGIN_MIN_INTERVAL: Duration = Duration::from_secs(65);

/// 进程内最近一次 SSO login 的发起时间（EIP / 知道共用——同源网关，限流按设备算）。
static LAST_SSO_LOGIN: LazyLock<Mutex<Option<Instant>>> = LazyLock::new(|| Mutex::new(None));

/// 发起 SSO login（`auth login` / `login`）前调用：距上次不足间隔则阻塞补齐，
/// 然后登记本次发起时间。在 spawn_blocking 上下文中调用（阻塞 sleep 安全）。
pub(crate) fn throttle_sso_login() {
    throttle_at(&LAST_SSO_LOGIN, SSO_LOGIN_MIN_INTERVAL);
}

fn throttle_at(last: &Mutex<Option<Instant>>, min_interval: Duration) {
    let mut guard = last.lock().unwrap_or_else(|e| e.into_inner());
    if let Some(t) = *guard {
        let elapsed = t.elapsed();
        if elapsed < min_interval {
            std::thread::sleep(min_interval - elapsed);
        }
    }
    *guard = Some(Instant::now());
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn second_begin_reuses_running_flow_with_cached_payload() {
        let f = SsoFlow::default();
        assert!(matches!(f.begin(), Begin::Start));
        f.arm_payload("https://sso/x".into(), Some("qr".into()));
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
        assert!(matches!(f.begin(), Begin::Start));
        match f.begin() {
            Begin::Reuse { payload } => assert!(payload.is_none()),
            _ => panic!("running flow must be reused even before payload is armed"),
        }
    }

    #[test]
    fn cancelled_flow_is_busy_until_it_finishes() {
        let f = SsoFlow::default();
        assert!(matches!(f.begin(), Begin::Start));
        f.request_cancel();
        assert!(f.is_cancelling());
        assert!(matches!(f.begin(), Begin::Busy));
        f.finish();
        assert!(!f.is_cancelling());
        assert!(matches!(f.begin(), Begin::Start));
    }

    #[test]
    fn cancel_on_idle_is_noop() {
        let f = SsoFlow::default();
        f.request_cancel();
        assert!(matches!(f.begin(), Begin::Start));
    }

    #[test]
    fn finish_clears_payload() {
        let f = SsoFlow::default();
        assert!(matches!(f.begin(), Begin::Start));
        f.arm_payload("https://sso/x".into(), None);
        f.finish();
        assert!(matches!(f.begin(), Begin::Start));
        match f.begin() {
            Begin::Reuse { payload } => assert!(payload.is_none()),
            _ => panic!("flow started after finish must run and have no stale payload"),
        }
    }

    #[test]
    fn throttle_enforces_min_interval_between_logins() {
        let last = Mutex::new(None);
        throttle_at(&last, Duration::from_millis(80));
        let t0 = Instant::now();
        throttle_at(&last, Duration::from_millis(80));
        assert!(
            t0.elapsed() >= Duration::from_millis(80),
            "second login inside the window must be delayed to the interval"
        );
        assert!(last.lock().unwrap().is_some());
    }

    #[test]
    fn throttle_passes_immediately_after_interval() {
        let stale = Mutex::new(Some(Instant::now() - Duration::from_secs(10)));
        let t0 = Instant::now();
        throttle_at(&stale, Duration::from_millis(80));
        assert!(
            t0.elapsed() < Duration::from_millis(80),
            "login outside the window must not be delayed"
        );
    }
}
