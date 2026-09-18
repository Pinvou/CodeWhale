//! DEV-13 adapter: plain-mode remote turns over `EnginePool`.
//!
//! The remote task owner stays engine-agnostic. This adapter is the only place
//! that knows how a plain chat turn is submitted, observed and cancelled, and
//! it never widens tool policy: the remote turn runs with the same product
//! defaults a local turn uses.

use std::sync::Arc;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use deepseek_tui::AppMode;
use tokio_util::sync::CancellationToken;

use crate::features::remote_control::controller::task::{
    RemoteChatTurnPort, TASK_CANCELLED_CODE,
};

use super::engine_pool::EnginePool;

const POLL_INTERVAL: Duration = Duration::from_millis(100);
const BUSY_GRACE: Duration = Duration::from_secs(5);

/// Narrow engine view used by the remote chat adapter. Keeping it separate
/// makes the adapter testable without a Tauri app handle.
#[async_trait]
pub(crate) trait RemoteChatEnginePort: Send + Sync {
    async fn send_user_message(
        &self,
        session_id: &str,
        content: String,
    ) -> std::result::Result<(), String>;

    fn is_turn_active(&self, session_id: &str) -> bool;

    async fn cancel_turn(&self, session_id: &str);
}

/// E2E-05：读取 B 会话最后一条 assistant 文本（用于把回复正文写进权威 journal
/// 并回传 A）。只在同进程内读取本地 transcript，不跨进程、不带任何凭证。
pub(crate) trait RemoteChatTranscriptPort: Send + Sync {
    fn last_assistant_text(&self, session_id: &str) -> Option<String>;
}

/// 基于 SessionStore 的实现：取最后一条 assistant 消息里的文本块，按 journal
/// 上限截断；任何读取失败都返回 None（不产生半事件）。
pub(crate) struct StoreChatTranscript {
    store: crate::features::sessions::SessionStore,
}

impl StoreChatTranscript {
    pub(crate) fn new(store: crate::features::sessions::SessionStore) -> Self {
        Self { store }
    }
}

impl RemoteChatTranscriptPort for StoreChatTranscript {
    fn last_assistant_text(&self, session_id: &str) -> Option<String> {
        use deepseek_tui::models::{ContentBlock, Role};

        let session = self.store.load(session_id).ok()?;
        let message = session
            .messages
            .iter()
            .rev()
            .find(|message| message.role == Role::Assistant)?;
        let mut text = String::new();
        for block in &message.content {
            if let ContentBlock::Text { text: block_text, .. } = block {
                text.push_str(block_text);
            }
        }
        let bounded: String = text
            .chars()
            .take(crate::features::remote_control::controller::journal::MAX_EVENT_TEXT_CHARS)
            .collect();
        if bounded.trim().is_empty() {
            None
        } else {
            Some(bounded)
        }
    }
}

pub(crate) struct EnginePoolChatEngine {
    pool: Arc<EnginePool>,
}

impl EnginePoolChatEngine {
    pub(crate) fn new(pool: Arc<EnginePool>) -> Self {
        Self { pool }
    }
}

#[async_trait]
impl RemoteChatEnginePort for EnginePoolChatEngine {
    async fn send_user_message(
        &self,
        session_id: &str,
        content: String,
    ) -> std::result::Result<(), String> {
        self.pool
            .send_user_message(session_id, content, AppMode::Agent, false)
            .await
            .map_err(|_| "task_engine_unavailable".to_owned())
    }

    fn is_turn_active(&self, session_id: &str) -> bool {
        self.pool.is_turn_active(session_id)
    }

    async fn cancel_turn(&self, session_id: &str) {
        let _ = self.pool.cancel(session_id, false).await;
    }
}

pub(crate) struct EnginePoolChatTurnPort {
    engine: Arc<dyn RemoteChatEnginePort>,
    transcript: Arc<dyn RemoteChatTranscriptPort>,
}

impl EnginePoolChatTurnPort {
    pub(crate) fn new(
        engine: Arc<dyn RemoteChatEnginePort>,
        transcript: Arc<dyn RemoteChatTranscriptPort>,
    ) -> Self {
        Self { engine, transcript }
    }

    async fn wait_for_terminal(
        &self,
        session_id: &str,
        cancel: &CancellationToken,
    ) -> std::result::Result<(), String> {
        let mut observed_busy = self.engine.is_turn_active(session_id);
        let started = Instant::now();
        loop {
            if cancel.is_cancelled() {
                self.engine.cancel_turn(session_id).await;
                return Err(TASK_CANCELLED_CODE.to_owned());
            }
            if self.engine.is_turn_active(session_id) {
                observed_busy = true;
            } else if observed_busy {
                return Ok(());
            } else if started.elapsed() >= BUSY_GRACE {
                return Err("task_turn_not_started".to_owned());
            }
            tokio::select! {
                _ = cancel.cancelled() => {
                    self.engine.cancel_turn(session_id).await;
                    return Err(TASK_CANCELLED_CODE.to_owned());
                }
                _ = tokio::time::sleep(POLL_INTERVAL) => {}
            }
        }
    }
}

#[async_trait]
impl RemoteChatTurnPort for EnginePoolChatTurnPort {
    async fn run_chat_turn(
        &self,
        session_id: &str,
        content: String,
        cancel: CancellationToken,
    ) -> std::result::Result<Option<String>, String> {
        self.engine.send_user_message(session_id, content).await?;
        self.wait_for_terminal(session_id, &cancel).await?;
        // 轮次结束后读回最后一条 assistant 文本作为正文（读取失败不阻塞终态）。
        Ok(self.transcript.last_assistant_text(session_id))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    use std::sync::Mutex;

    #[derive(Default)]
    struct FakeEngine {
        sent: Mutex<Vec<String>>,
        active: AtomicBool,
        flips: AtomicUsize,
        cancelled: Mutex<Vec<String>>,
        completes_after_first_poll: bool,
    }

    impl FakeEngine {
        fn immediate() -> Arc<Self> {
            Arc::new(Self {
                completes_after_first_poll: true,
                ..Self::default()
            })
        }

        fn always_busy() -> Arc<Self> {
            let fake = Self {
                completes_after_first_poll: false,
                ..Self::default()
            };
            fake.active.store(true, Ordering::SeqCst);
            Arc::new(fake)
        }
    }

    #[async_trait]
    impl RemoteChatEnginePort for FakeEngine {
        async fn send_user_message(
            &self,
            _session_id: &str,
            content: String,
        ) -> std::result::Result<(), String> {
            self.sent.lock().unwrap().push(content);
            self.active.store(true, Ordering::SeqCst);
            Ok(())
        }

        fn is_turn_active(&self, _session_id: &str) -> bool {
            // The first observation reports busy, the second one completes the
            // turn so the adapter can prove it waits for a real transition.
            if !self.completes_after_first_poll {
                self.active.load(Ordering::SeqCst)
            } else if self.flips.fetch_add(1, Ordering::SeqCst) == 0 {
                self.active.load(Ordering::SeqCst)
            } else {
                false
            }
        }

        async fn cancel_turn(&self, session_id: &str) {
            self.cancelled.lock().unwrap().push(session_id.to_owned());
        }
    }

    fn runtime() -> tokio::runtime::Runtime {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
    }

    /// 测试用 transcript 端口：固定返回一句回复（或 None）。
    struct FakeTranscript(Option<&'static str>);

    impl RemoteChatTranscriptPort for FakeTranscript {
        fn last_assistant_text(&self, _session_id: &str) -> Option<String> {
            self.0.map(str::to_owned)
        }
    }

    #[test]
    fn turn_is_submitted_and_completion_waits_for_the_busy_transition() {
        let fake = FakeEngine::immediate();
        let port = EnginePoolChatTurnPort::new(
            fake.clone(),
            Arc::new(FakeTranscript(Some("remote reply"))),
        );
        let reply = runtime()
            .block_on(port.run_chat_turn(
                "session-0001",
                "hello remote".into(),
                CancellationToken::new(),
            ))
            .unwrap();
        assert_eq!(fake.sent.lock().unwrap().as_slice(), ["hello remote"]);
        assert_eq!(reply.as_deref(), Some("remote reply"));
    }

    #[test]
    fn cancellation_stops_the_turn_and_reports_the_cancel_code() {
        let fake = FakeEngine::always_busy();
        let port = EnginePoolChatTurnPort::new(fake.clone(), Arc::new(FakeTranscript(None)));
        let cancel = CancellationToken::new();
        let token = cancel.clone();
        let result = runtime().block_on(async move {
            let task = tokio::spawn(async move {
                port.run_chat_turn("session-0001", "hello".into(), cancel)
                    .await
            });
            tokio::time::sleep(Duration::from_millis(20)).await;
            token.cancel();
            task.await.unwrap()
        });
        assert_eq!(result.unwrap_err(), TASK_CANCELLED_CODE);
        assert_eq!(fake.cancelled.lock().unwrap().as_slice(), ["session-0001"]);
    }
}
