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

use crate::features::remote_control::controller::task::{RemoteChatTurnPort, TASK_CANCELLED_CODE};
use crate::features::remote_control::controller::turn_budget::{
    CANCEL_SETTLE_BUDGET, TURN_TIMEOUT_CODE, TurnBudget,
};

use super::engine::TurnReservation;
use super::engine_pool::EnginePool;

const POLL_INTERVAL: Duration = Duration::from_millis(100);
const BUSY_GRACE: Duration = Duration::from_secs(5);

/// Narrow engine view used by the remote chat adapter. Keeping it separate
/// makes the adapter testable without a Tauri app handle.
#[async_trait]
pub(crate) trait RemoteChatEnginePort: Send + Sync {
    fn reserve_turn(&self, session_id: &str) -> Result<RemoteChatReservation, String>;

    async fn send_user_message(
        &self,
        session_id: &str,
        content: String,
        reservation: RemoteChatReservation,
    ) -> std::result::Result<(), String>;

    fn is_turn_active(&self, session_id: &str, generation: u64) -> bool;

    async fn cancel_turn(&self, session_id: &str, generation: u64);
}

pub(crate) struct RemoteChatReservation {
    generation: u64,
    reservation: Option<TurnReservation>,
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
    fn reserve_turn(&self, session_id: &str) -> Result<RemoteChatReservation, String> {
        let reservation = self
            .pool
            .reserve_turn(session_id)
            .map_err(|_| "task_session_busy".to_owned())?;
        let generation = reservation
            .generation()
            .map_err(|_| "task_turn_not_started".to_owned())?;
        Ok(RemoteChatReservation {
            generation,
            reservation: Some(reservation),
        })
    }

    async fn send_user_message(
        &self,
        session_id: &str,
        content: String,
        reservation: RemoteChatReservation,
    ) -> std::result::Result<(), String> {
        self.pool
            .send_admitted_user_message(
                session_id,
                content,
                AppMode::Agent,
                false,
                reservation
                    .reservation
                    .ok_or_else(|| "task_turn_not_started".to_owned())?,
            )
            .await
            .map_err(|_| "task_engine_unavailable".to_owned())
    }

    fn is_turn_active(&self, session_id: &str, generation: u64) -> bool {
        self.pool.is_remote_turn_active(session_id, generation)
    }

    async fn cancel_turn(&self, session_id: &str, generation: u64) {
        let _ = self.pool.cancel_remote_turn(session_id, generation).await;
    }
}

pub(crate) struct EnginePoolChatTurnPort {
    engine: Arc<dyn RemoteChatEnginePort>,
}

impl EnginePoolChatTurnPort {
    pub(crate) fn new(engine: Arc<dyn RemoteChatEnginePort>) -> Self {
        Self { engine }
    }

    async fn wait_for_terminal(
        &self,
        session_id: &str,
        cancel: &CancellationToken,
        generation: u64,
    ) -> std::result::Result<(), String> {
        let mut observed_busy = self.engine.is_turn_active(session_id, generation);
        let started = Instant::now();
        loop {
            if cancel.is_cancelled() {
                return Err(TASK_CANCELLED_CODE.to_owned());
            }
            if self.engine.is_turn_active(session_id, generation) {
                observed_busy = true;
            } else if observed_busy {
                return Ok(());
            } else if started.elapsed() >= BUSY_GRACE {
                return Err("task_turn_not_started".to_owned());
            }
            tokio::select! {
                _ = cancel.cancelled() => {
                    return Err(TASK_CANCELLED_CODE.to_owned());
                }
                _ = tokio::time::sleep(POLL_INTERVAL) => {}
            }
        }
    }
}

impl EnginePoolChatTurnPort {
    async fn run_chat_turn_with_budget(
        &self,
        session_id: &str,
        content: String,
        cancel: CancellationToken,
        budget: TurnBudget,
    ) -> std::result::Result<(), String> {
        if cancel.is_cancelled() {
            return Err(TASK_CANCELLED_CODE.into());
        }
        let reservation = self.engine.reserve_turn(session_id)?;
        let generation = reservation.generation;
        let result = budget
            .run(&cancel, async {
                self.engine
                    .send_user_message(session_id, content, reservation)
                    .await?;
                self.wait_for_terminal(session_id, &cancel, generation)
                    .await
            })
            .await;
        if matches!(
            result.as_ref().err().map(String::as_str),
            Some(TASK_CANCELLED_CODE | TURN_TIMEOUT_CODE)
        ) {
            // Never call session-wide cancel. A newer reservation/runtime must
            // remain untouched even if cleanup resumes after the owned turn ends.
            let _ = tokio::time::timeout(
                CANCEL_SETTLE_BUDGET,
                self.engine.cancel_turn(session_id, generation),
            )
            .await;
        }
        result
    }
}

#[async_trait]
impl RemoteChatTurnPort for EnginePoolChatTurnPort {
    async fn run_chat_turn(
        &self,
        session_id: &str,
        content: String,
        cancel: CancellationToken,
    ) -> Result<(), String> {
        let budget = TurnBudget::from_environment()?;
        self.run_chat_turn_with_budget(session_id, content, cancel, budget)
            .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

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
        fn reserve_turn(&self, _session_id: &str) -> Result<RemoteChatReservation, String> {
            Ok(RemoteChatReservation {
                generation: 1,
                reservation: None,
            })
        }
        async fn send_user_message(
            &self,
            _session_id: &str,
            content: String,
            _reservation: RemoteChatReservation,
        ) -> std::result::Result<(), String> {
            self.sent.lock().unwrap().push(content);
            self.active.store(true, Ordering::SeqCst);
            Ok(())
        }

        fn is_turn_active(&self, _session_id: &str, _generation: u64) -> bool {
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

        async fn cancel_turn(&self, session_id: &str, generation: u64) {
            assert_eq!(generation, 1);
            self.cancelled.lock().unwrap().push(session_id.to_owned());
        }
    }

    fn runtime() -> tokio::runtime::Runtime {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
    }

    #[test]
    fn turn_is_submitted_and_completion_waits_for_the_busy_transition() {
        let fake = FakeEngine::immediate();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        runtime()
            .block_on(port.run_chat_turn(
                "session-0001",
                "hello remote".into(),
                CancellationToken::new(),
            ))
            .unwrap();
        assert_eq!(fake.sent.lock().unwrap().as_slice(), ["hello remote"]);
    }

    #[test]
    fn cancellation_stops_the_turn_and_reports_the_cancel_code() {
        let fake = FakeEngine::always_busy();
        let port = EnginePoolChatTurnPort::new(fake.clone());
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

    #[tokio::test(start_paused = true)]
    async fn busy_turn_reaches_its_frozen_total_deadline() {
        let fake = FakeEngine::always_busy();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        let result = port
            .run_chat_turn_with_budget(
                "session-0001",
                "hello".into(),
                CancellationToken::new(),
                TurnBudget::for_test(Duration::from_secs(1800)),
            )
            .await;
        assert_eq!(result.unwrap_err(), TURN_TIMEOUT_CODE);
        assert_eq!(fake.sent.lock().unwrap().as_slice(), ["hello"]);
        assert_eq!(fake.cancelled.lock().unwrap().as_slice(), ["session-0001"]);
    }

    #[tokio::test(start_paused = true)]
    async fn precancelled_turn_is_never_admitted_or_submitted() {
        let fake = FakeEngine::always_busy();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        let cancel = CancellationToken::new();
        cancel.cancel();
        let result = port
            .run_chat_turn_with_budget(
                "session-0001",
                "hello".into(),
                cancel,
                TurnBudget::for_test(Duration::from_secs(1)),
            )
            .await;
        assert_eq!(result.unwrap_err(), TASK_CANCELLED_CODE);
        assert!(fake.sent.lock().unwrap().is_empty());
        assert!(fake.cancelled.lock().unwrap().is_empty());
    }
}
