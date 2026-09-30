//! DEV-13 adapter: plain-mode remote turns over `EnginePool`.
//!
//! The remote task owner stays engine-agnostic. This adapter is the only place
//! that knows how a plain chat turn is submitted, observed and cancelled, and
//! it never widens tool policy: the remote turn runs with the same product
//! defaults a local turn uses.

use std::sync::Arc;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use parking_lot::Mutex;
use serde_json::{Value, json};
use tauri::Listener as _;
use tokio_util::sync::CancellationToken;

use crate::features::remote_control::controller::journal::{
    JournalKind, MAX_EVENT_TEXT_CHARS, StreamingTextSanitizer,
};
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::task::{
    ControllerTaskEventSink, RemoteChatTurnPort, TASK_CANCELLED_CODE, TASK_MODEL_SOURCE_CONTROLLED,
    TASK_MODEL_SOURCE_TEMPORARY,
};
use crate::features::remote_control::controller::temporary_model::SharedTemporaryModelStore;

use super::engine_pool::{EnginePool, RemoteRuntimeModelBundle};

const POLL_INTERVAL: Duration = Duration::from_millis(100);
const BUSY_GRACE: Duration = Duration::from_secs(5);
const REASONING_FLUSH_INTERVAL: Duration = Duration::from_millis(500);
const REASONING_BATCH_CHARS: usize = 2_048;
// The frontend retains 200 transcript entries. Keeping a turn below 64
// reasoning batches ensures two consecutive turns cannot evict each other just
// from thinking updates.
const MAX_REASONING_EVENTS_PER_TURN: usize = 64;
const MAX_REASONING_CHARS_PER_TURN: usize = 64 * 1_024;

/// Narrow engine view used by the remote chat adapter. Keeping it separate
/// makes the adapter testable without a Tauri app handle.
#[async_trait]
pub(crate) trait RemoteChatEnginePort: Send + Sync {
    async fn send_user_message(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        relay: Option<&TurnEventRelayHandle>,
    ) -> std::result::Result<(), String>;

    fn is_turn_active(&self, session_id: &str) -> bool;

    async fn cancel_turn(&self, session_id: &str);

    async fn release_task_model(&self, session_id: &str, model_source: &str);

    fn start_turn_relay(
        &self,
        _session_id: &str,
        _task_id: &str,
        _events: Arc<dyn ControllerTaskEventSink>,
    ) -> Option<TurnEventRelayHandle> {
        None
    }
}

#[derive(Default)]
struct TurnEventRelayState {
    generation: Option<u64>,
    reasoning_sanitizer: StreamingTextSanitizer,
    reasoning_ready: String,
    flush_scheduled: bool,
    stopped: bool,
    emitted_events: usize,
    emitted_chars: usize,
    assistant_text: Option<String>,
}

impl TurnEventRelayState {
    fn push_reasoning(&mut self, generation: Option<u64>, text: &str) -> Option<(bool, bool)> {
        if self.stopped
            || generation != self.generation
            || self.emitted_events >= MAX_REASONING_EVENTS_PER_TURN
            || self.emitted_chars >= MAX_REASONING_CHARS_PER_TURN
        {
            return None;
        }
        let sanitized = self.reasoning_sanitizer.push(text);
        self.reasoning_ready.push_str(&sanitized);
        let flush_now = self.reasoning_ready.chars().count() >= REASONING_BATCH_CHARS;
        let schedule = !flush_now && !self.flush_scheduled;
        if schedule {
            self.flush_scheduled = true;
        }
        Some((flush_now, schedule))
    }
}

pub(crate) struct TurnEventRelayHandle {
    app: tauri::AppHandle,
    listener_ids: Vec<u32>,
    state: Arc<Mutex<TurnEventRelayState>>,
    events: Arc<dyn ControllerTaskEventSink>,
    session_id: String,
    task_id: String,
}

impl TurnEventRelayHandle {
    fn arm(&self, generation: u64) {
        self.state.lock().generation = Some(generation);
    }

    fn finish(&mut self) -> Option<String> {
        for listener_id in self.listener_ids.drain(..) {
            self.app.unlisten(listener_id);
        }
        let (reasoning_batches, assistant) = {
            let mut state = self.state.lock();
            if state.stopped {
                return state.assistant_text.take();
            }
            state.stopped = true;
            let tail = state.reasoning_sanitizer.finish();
            state.reasoning_ready.push_str(&tail);
            let mut reasoning_batches = Vec::new();
            while let Some(batch) = take_reasoning_batch(&mut state, true) {
                reasoning_batches.push(batch);
            }
            let assistant = state.assistant_text.take().and_then(|text| {
                let bounded = text.chars().take(MAX_EVENT_TEXT_CHARS).collect::<String>();
                (!bounded.trim().is_empty()).then_some(bounded)
            });
            (reasoning_batches, assistant)
        };
        for text in reasoning_batches {
            self.events.emit_journal(
                JournalKind::Reasoning,
                &self.session_id,
                Some(&self.task_id),
                json!({ "text": text }),
            );
        }
        assistant
    }
}

impl Drop for TurnEventRelayHandle {
    fn drop(&mut self) {
        let _ = self.finish();
    }
}

fn take_prefix_chars(text: &mut String, limit: usize) -> String {
    let split = text
        .char_indices()
        .nth(limit)
        .map(|(index, _)| index)
        .unwrap_or(text.len());
    text.drain(..split).collect()
}

fn take_reasoning_batch(state: &mut TurnEventRelayState, force: bool) -> Option<String> {
    if state.emitted_events >= MAX_REASONING_EVENTS_PER_TURN
        || state.emitted_chars >= MAX_REASONING_CHARS_PER_TURN
    {
        state.reasoning_ready.clear();
        return None;
    }
    let available = state.reasoning_ready.chars().count();
    if available == 0 || (!force && available < REASONING_BATCH_CHARS) {
        return None;
    }
    let remaining = MAX_REASONING_CHARS_PER_TURN - state.emitted_chars;
    let limit = available.min(REASONING_BATCH_CHARS).min(remaining);
    let batch = take_prefix_chars(&mut state.reasoning_ready, limit);
    state.emitted_events += 1;
    state.emitted_chars += batch.chars().count();
    Some(batch)
}

fn flush_reasoning(
    state: &Arc<Mutex<TurnEventRelayState>>,
    events: &Arc<dyn ControllerTaskEventSink>,
    session_id: &str,
    task_id: &str,
    force: bool,
) {
    // Keep the state lock through delivery. `finish` takes the same lock after
    // unlistening, so a timer that already removed a batch cannot publish it
    // after the final assistant event has been journaled.
    let mut state = state.lock();
    state.flush_scheduled = false;
    let batch = if state.stopped && !force {
        None
    } else {
        take_reasoning_batch(&mut state, force)
    };
    if let Some(text) = batch {
        events.emit_journal(
            JournalKind::Reasoning,
            session_id,
            Some(task_id),
            json!({ "text": text }),
        );
    }
}

pub(crate) struct EnginePoolChatEngine {
    app: tauri::AppHandle,
    pool: Arc<EnginePool>,
    models: SharedTemporaryModelStore,
}

impl EnginePoolChatEngine {
    pub(crate) fn new(
        app: tauri::AppHandle,
        pool: Arc<EnginePool>,
        models: SharedTemporaryModelStore,
    ) -> Self {
        Self { app, pool, models }
    }
}

#[async_trait]
impl RemoteChatEnginePort for EnginePoolChatEngine {
    async fn send_user_message(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        relay: Option<&TurnEventRelayHandle>,
    ) -> std::result::Result<(), String> {
        let model_override = match model_source {
            TASK_MODEL_SOURCE_CONTROLLED => None,
            TASK_MODEL_SOURCE_TEMPORARY => {
                let models = self
                    .models
                    .runtime_models_for_task(task_id, lease)
                    .await
                    .map_err(|error| error.code.to_owned())?;
                Some(
                    RemoteRuntimeModelBundle::new(models.primary, models.vision)
                        .map_err(|_| "model_runtime_invalid".to_owned())?,
                )
            }
            _ => return Err("task_model_source_invalid".to_owned()),
        };
        self.pool
            .send_remote_user_message(session_id, content, model_override, |generation| {
                if let Some(relay) = relay {
                    relay.arm(generation);
                }
            })
            .await
            .map_err(|_| "task_engine_unavailable".to_owned())
    }

    fn is_turn_active(&self, session_id: &str) -> bool {
        self.pool.is_turn_active(session_id)
    }

    async fn cancel_turn(&self, session_id: &str) {
        let _ = self.pool.cancel(session_id, false).await;
    }

    async fn release_task_model(&self, session_id: &str, model_source: &str) {
        if model_source == TASK_MODEL_SOURCE_TEMPORARY {
            self.pool.evict(session_id).await;
        }
    }

    fn start_turn_relay(
        &self,
        session_id: &str,
        task_id: &str,
        events: Arc<dyn ControllerTaskEventSink>,
    ) -> Option<TurnEventRelayHandle> {
        let expected_session_id = session_id.to_owned();
        let state = Arc::new(Mutex::new(TurnEventRelayState::default()));
        let reasoning_state = Arc::clone(&state);
        let reasoning_events = Arc::clone(&events);
        let reasoning_session_id = session_id.to_owned();
        let reasoning_task_id = task_id.to_owned();
        let reasoning_listener = self.app.listen("chat:reasoning_delta", move |event| {
            let Ok(payload) = serde_json::from_str::<Value>(event.payload()) else {
                return;
            };
            if payload.get("session_id").and_then(Value::as_str)
                != Some(expected_session_id.as_str())
            {
                return;
            }
            let Some(text) = payload
                .get("text")
                .and_then(Value::as_str)
                .filter(|text| !text.is_empty())
            else {
                return;
            };
            let generation = payload.get("generation").and_then(Value::as_u64);
            let Some((flush_now, schedule)) = ({
                let mut state = reasoning_state.lock();
                state.push_reasoning(generation, text)
            }) else {
                return;
            };
            if flush_now {
                flush_reasoning(
                    &reasoning_state,
                    &reasoning_events,
                    &reasoning_session_id,
                    &reasoning_task_id,
                    false,
                );
            } else if schedule {
                let state = Arc::clone(&reasoning_state);
                let events = Arc::clone(&reasoning_events);
                let session_id = reasoning_session_id.clone();
                let task_id = reasoning_task_id.clone();
                tauri::async_runtime::spawn(async move {
                    tokio::time::sleep(REASONING_FLUSH_INTERVAL).await;
                    flush_reasoning(&state, &events, &session_id, &task_id, true);
                });
            }
        });

        let assistant_state = Arc::clone(&state);
        let assistant_session_id = session_id.to_owned();
        let assistant_listener = self.app.listen("chat:remote_turn_assistant", move |event| {
            let Ok(payload) = serde_json::from_str::<Value>(event.payload()) else {
                return;
            };
            if payload.get("session_id").and_then(Value::as_str)
                != Some(assistant_session_id.as_str())
            {
                return;
            }
            let generation = payload.get("generation").and_then(Value::as_u64);
            let Some(text) = payload
                .get("text")
                .and_then(Value::as_str)
                .filter(|text| !text.trim().is_empty())
            else {
                return;
            };
            let mut state = assistant_state.lock();
            if !state.stopped && generation == state.generation {
                state.assistant_text = Some(text.to_owned());
            }
        });

        Some(TurnEventRelayHandle {
            app: self.app.clone(),
            listener_ids: vec![reasoning_listener, assistant_listener],
            state,
            events,
            session_id: session_id.to_owned(),
            task_id: task_id.to_owned(),
        })
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
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<Option<String>, String> {
        let mut turn_relay = self.engine.start_turn_relay(session_id, task_id, events);
        let result = async {
            tokio::select! {
                biased;
                _ = cancel.cancelled() => {
                    self.engine.cancel_turn(session_id).await;
                    return Err(TASK_CANCELLED_CODE.to_owned());
                }
                result = self.engine.send_user_message(
                    session_id,
                    task_id,
                    model_source,
                    lease,
                    content,
                    turn_relay.as_ref(),
                ) => result?,
            }
            self.wait_for_terminal(session_id, &cancel).await?;
            Ok(())
        }
        .await;
        // Finish before ChatTurnExecutor emits the assistant journal entry, so
        // every pending reasoning batch remains ordered before the final answer.
        let assistant_text = turn_relay.as_mut().and_then(TurnEventRelayHandle::finish);
        // EnginePool entries retain their prepared runtime credential. A
        // task-scoped A credential must therefore evict the engine on every
        // success/failure/cancel path before the model store is released.
        self.engine
            .release_task_model(session_id, model_source)
            .await;
        result.map(|()| assistant_text)
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
        sources: Mutex<Vec<String>>,
        released: Mutex<Vec<String>>,
        active: AtomicBool,
        flips: AtomicUsize,
        cancelled: Mutex<Vec<String>>,
        completes_after_first_poll: bool,
        send_started: Option<Arc<tokio::sync::Notify>>,
        send_release: Option<Arc<tokio::sync::Notify>>,
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
            _task_id: &str,
            model_source: &str,
            _lease: &LeaseContext,
            content: String,
            _relay: Option<&TurnEventRelayHandle>,
        ) -> std::result::Result<(), String> {
            if let Some(started) = self.send_started.as_ref() {
                started.notify_one();
            }
            if let Some(release) = self.send_release.as_ref() {
                release.notified().await;
            }
            self.sent.lock().unwrap().push(content);
            self.sources.lock().unwrap().push(model_source.to_owned());
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

        async fn release_task_model(&self, session_id: &str, model_source: &str) {
            if model_source == TASK_MODEL_SOURCE_TEMPORARY {
                self.released.lock().unwrap().push(session_id.to_owned());
            }
        }
    }

    fn runtime() -> tokio::runtime::Runtime {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
    }

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    struct NoopEvents;

    impl ControllerTaskEventSink for NoopEvents {
        fn emit(&self, _event: Value) {}
    }

    fn events() -> Arc<dyn ControllerTaskEventSink> {
        Arc::new(NoopEvents)
    }

    #[test]
    fn turn_is_submitted_and_completion_waits_for_the_busy_transition() {
        let fake = FakeEngine::immediate();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        let reply = runtime()
            .block_on(port.run_chat_turn(
                "session-0001",
                "task-0001",
                TASK_MODEL_SOURCE_CONTROLLED,
                &lease(),
                "hello remote".into(),
                events(),
                CancellationToken::new(),
            ))
            .unwrap();
        assert_eq!(fake.sent.lock().unwrap().as_slice(), ["hello remote"]);
        assert_eq!(reply, None, "the fake engine does not install a turn relay");
    }

    #[test]
    fn cancellation_stops_the_turn_and_reports_the_cancel_code() {
        let fake = FakeEngine::always_busy();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        let cancel = CancellationToken::new();
        let token = cancel.clone();
        let result = runtime().block_on(async move {
            let task = tokio::spawn(async move {
                port.run_chat_turn(
                    "session-0001",
                    "task-0001",
                    TASK_MODEL_SOURCE_CONTROLLED,
                    &lease(),
                    "hello".into(),
                    events(),
                    cancel,
                )
                .await
            });
            tokio::time::sleep(Duration::from_millis(20)).await;
            token.cancel();
            task.await.unwrap()
        });
        assert_eq!(result.unwrap_err(), TASK_CANCELLED_CODE);
        assert_eq!(fake.cancelled.lock().unwrap().as_slice(), ["session-0001"]);
    }

    #[test]
    fn cancellation_drops_a_pending_model_prepare_before_turn_submission() {
        let send_started = Arc::new(tokio::sync::Notify::new());
        let send_release = Arc::new(tokio::sync::Notify::new());
        let fake = Arc::new(FakeEngine {
            send_started: Some(send_started.clone()),
            send_release: Some(send_release),
            ..FakeEngine::default()
        });
        let port = EnginePoolChatTurnPort::new(fake.clone());
        let cancel = CancellationToken::new();
        let trigger = cancel.clone();
        let result = runtime().block_on(async move {
            let task = tokio::spawn(async move {
                port.run_chat_turn(
                    "session-0001",
                    "task-0001",
                    TASK_MODEL_SOURCE_TEMPORARY,
                    &lease(),
                    "must-not-send".into(),
                    events(),
                    cancel,
                )
                .await
            });
            send_started.notified().await;
            trigger.cancel();
            task.await.unwrap()
        });

        assert_eq!(result.unwrap_err(), TASK_CANCELLED_CODE);
        assert!(fake.sent.lock().unwrap().is_empty());
        assert_eq!(fake.cancelled.lock().unwrap().as_slice(), ["session-0001"]);
        assert_eq!(fake.released.lock().unwrap().as_slice(), ["session-0001"]);
    }

    #[test]
    fn temporary_model_source_reaches_the_engine_and_is_released() {
        let fake = FakeEngine::immediate();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        runtime()
            .block_on(port.run_chat_turn(
                "session-0001",
                "task-0001",
                TASK_MODEL_SOURCE_TEMPORARY,
                &lease(),
                "hello".into(),
                events(),
                CancellationToken::new(),
            ))
            .unwrap();
        assert_eq!(
            fake.sources.lock().unwrap().as_slice(),
            [TASK_MODEL_SOURCE_TEMPORARY]
        );
        assert_eq!(fake.released.lock().unwrap().as_slice(), ["session-0001"]);
    }

    #[test]
    fn reasoning_state_rejects_foreign_generations() {
        let mut state = TurnEventRelayState {
            generation: Some(7),
            ..TurnEventRelayState::default()
        };
        assert_eq!(state.push_reasoning(Some(6), "must-not-leak "), None);
        assert!(state.reasoning_ready.is_empty());
        assert!(state.push_reasoning(Some(7), "accepted ").is_some());
        assert_eq!(state.reasoning_ready, "accepted ");
    }

    #[test]
    fn reasoning_batches_are_bounded_per_turn() {
        let mut state = TurnEventRelayState {
            reasoning_ready: "x".repeat(MAX_REASONING_CHARS_PER_TURN * 2),
            ..TurnEventRelayState::default()
        };
        let mut batches = Vec::new();
        while let Some(batch) = take_reasoning_batch(&mut state, true) {
            batches.push(batch);
        }
        assert!(batches.len() <= MAX_REASONING_EVENTS_PER_TURN);
        assert_eq!(
            batches
                .iter()
                .map(|batch| batch.chars().count())
                .sum::<usize>(),
            MAX_REASONING_CHARS_PER_TURN
        );
        assert!(state.reasoning_ready.is_empty());
    }
}
