//! DEV-13 adapter: plain-mode remote turns over `EnginePool`.
//!
//! The remote task owner stays engine-agnostic. This adapter is the only place
//! that knows how a plain chat turn is submitted, observed and cancelled, and
//! it never widens tool policy: the remote turn runs with the same product
//! defaults a local turn uses.

use std::sync::Arc;

use super::skill_materialization::RemoteTurnOwner;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use parking_lot::Mutex;
use serde_json::{Value, json};
use tauri::Listener as _;
use tokio_util::sync::CancellationToken;

use crate::features::remote_control::controller::journal::{
    JournalKind, MAX_EVENT_TEXT_CHARS, StreamingTextSanitizer, sanitize_text,
};
use crate::features::remote_control::controller::protocol::LeaseContext;
// Production and cfg(test) pipeline regressions share this existing task boundary.
// Rust cannot cfg-gate individual grouped-use items. Expect unused test types only
// in non-test builds; test builds retain the strict import check.
#[cfg_attr(
    not(test),
    expect(
        unused_imports,
        reason = "task pipeline regression types are test-only"
    )
)]
use crate::features::remote_control::controller::task::{
    AdmissionOutcome, ChatTurnExecutor, ControllerTaskEventSink, PublishedAttachment,
    RemoteChatTurnPort, RemoteTaskLedger, RemoteTaskService, RemoteTaskState, TASK_CANCELLED_CODE,
    TASK_MODE_CHAT, TASK_MODEL_SOURCE_CONTROLLED, TASK_MODEL_SOURCE_TEMPORARY,
    task_input_digest_for_value,
};
use crate::features::remote_control::controller::temporary_model::SharedTemporaryModelStore;

use super::engine::TurnReservation;
use super::engine_pool::{EnginePool, RemoteRuntimeModelBundle};

pub(crate) struct RemoteChatReservation {
    turn: Option<TurnReservation>,
    generation: u64,
}

impl RemoteChatReservation {
    fn take_turn(&mut self) -> Option<TurnReservation> {
        self.turn.take()
    }
}

fn reserve_remote_chat_turn(
    session_id: &str,
    reserve: impl FnOnce() -> std::result::Result<TurnReservation, String>,
) -> std::result::Result<RemoteChatReservation, String> {
    crate::features::sessions::validate_session_id(session_id)
        .map_err(|_| "task_session_invalid".to_owned())?;
    // Occupy the real turn gate before any model, Skills or attachment work.
    let turn = reserve()?;
    let generation = turn.generation();
    Ok(RemoteChatReservation {
        turn: Some(turn),
        generation,
    })
}
use crate::features::remote_control::controller::turn_budget::{
    CANCEL_SETTLE_BUDGET, TURN_TIMEOUT_CODE, TurnBudget,
};

const POLL_INTERVAL: Duration = Duration::from_millis(100);
const BUSY_GRACE: Duration = Duration::from_secs(5);
const REASONING_FLUSH_INTERVAL: Duration = Duration::from_millis(500);
const REASONING_BATCH_CHARS: usize = 2_048;
// The frontend retains 200 transcript entries. Keeping a turn below 64
// reasoning batches ensures two consecutive turns cannot evict each other just
// from thinking updates.
const MAX_REASONING_EVENTS_PER_TURN: usize = 64;
const MAX_REASONING_CHARS_PER_TURN: usize = 64 * 1_024;
const MAX_ASSISTANT_EVENTS_PER_TURN: usize = 256;
const MAX_ASSISTANT_CHARS_PER_TURN: usize = 128 * 1_024;

/// Narrow engine view used by the remote chat adapter. Keeping it separate
/// makes the adapter testable without a Tauri app handle.
#[async_trait]
pub(crate) trait RemoteChatEnginePort: Send + Sync {
    fn reserve_remote_turn(
        &self,
        _session_id: &str,
    ) -> std::result::Result<Option<RemoteChatReservation>, String> {
        Ok(None)
    }
    async fn send_user_message(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
        relay: Option<&TurnEventRelayHandle>,
        owner: Arc<RemoteTurnOwner>,
        reserved: &mut Option<RemoteChatReservation>,
    ) -> std::result::Result<(), String>;

    async fn send_user_message_with_attachments(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
        attachments: Vec<PublishedAttachment>,
        relay: Option<&TurnEventRelayHandle>,
        owner: Arc<RemoteTurnOwner>,
        reserved: &mut Option<RemoteChatReservation>,
    ) -> std::result::Result<(), String> {
        if !attachments.is_empty() {
            return Err("task_attachments_unavailable".to_owned());
        }
        self.send_user_message(
            session_id,
            task_id,
            model_source,
            lease,
            content,
            skill_ids,
            relay,
            owner,
            reserved,
        )
        .await
    }

    fn is_turn_active(&self, session_id: &str, generation: Option<u64>) -> bool;

    /// Only the authoritative terminal for this reservation can complete a
    /// remote task. An inactive engine is not proof of success.
    fn terminal_result(
        &self,
        session_id: &str,
        relay: Option<&TurnEventRelayHandle>,
    ) -> Option<std::result::Result<(), String>>;

    async fn cancel_turn(&self, session_id: &str, generation: Option<u64>);

    async fn release_task_model(&self, session_id: &str, owner: &Arc<RemoteTurnOwner>);

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
    terminal: Option<std::result::Result<(), String>>,
    reasoning_sanitizer: StreamingTextSanitizer,
    reasoning_ready: String,
    flush_scheduled: bool,
    stopped: bool,
    emitted_events: usize,
    emitted_chars: usize,
    assistant_sanitizer: StreamingTextSanitizer,
    assistant_events: usize,
    assistant_chars: usize,
}

impl TurnEventRelayState {
    fn accept_terminal(&mut self, session_id: &str, payload: &Value) {
        if self.stopped
            || self.terminal.is_some()
            || self.generation.is_none()
            || payload.get("session_id").and_then(Value::as_str) != Some(session_id)
            || payload.get("generation").and_then(Value::as_u64) != self.generation
        {
            return;
        }
        // Never return raw provider errors, credentials or B-local paths to A.
        let result = match payload.get("status").and_then(Value::as_str) {
            Some("Completed")
                if payload.get("error").is_some_and(Value::is_null)
                    && payload.get("shell_cleanup_failed").and_then(Value::as_bool)
                        == Some(false)
                    && payload.get("operation_rejected").and_then(Value::as_bool)
                        == Some(false) =>
            {
                Ok(())
            }
            Some("Interrupted") => Err(TASK_CANCELLED_CODE.to_owned()),
            _ => Err("task_turn_failed".to_owned()),
        };
        self.terminal = Some(result);
    }

    fn push_reasoning(&mut self, generation: Option<u64>, text: &str) -> Option<(bool, bool)> {
        if self.stopped
            || self.terminal.is_some()
            || self.generation.is_none()
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

    fn push_assistant(&mut self, text: &str) -> Option<String> {
        if self.stopped
            || self.terminal.is_some()
            || self.generation.is_none()
            || self.assistant_events >= MAX_ASSISTANT_EVENTS_PER_TURN
            || self.assistant_chars >= MAX_ASSISTANT_CHARS_PER_TURN
        {
            return None;
        }
        let sanitized = self.assistant_sanitizer.push(text);
        self.bound_assistant(sanitized)
    }

    fn finish_assistant_token(&mut self, completed: bool) -> Option<String> {
        let tail = if completed {
            self.assistant_sanitizer.finish_completed()
        } else {
            self.assistant_sanitizer.abort_pending()
        };
        self.bound_assistant(tail)
    }

    fn close_streams(&mut self, completed: bool) -> (Vec<String>, Option<String>) {
        if self.stopped {
            return (Vec::new(), None);
        }
        self.stopped = true;
        let completed = completed && matches!(self.terminal, Some(Ok(())));
        let tail = if completed {
            self.reasoning_sanitizer.finish_completed()
        } else {
            self.reasoning_sanitizer.abort_pending()
        };
        self.reasoning_ready.push_str(&tail);
        let mut batches = Vec::new();
        while let Some(batch) = take_reasoning_batch(self, true) {
            batches.push(batch);
        }
        (batches, self.finish_assistant_token(completed))
    }

    fn bound_assistant(&mut self, text: String) -> Option<String> {
        if text.is_empty()
            || self.assistant_events >= MAX_ASSISTANT_EVENTS_PER_TURN
            || self.assistant_chars >= MAX_ASSISTANT_CHARS_PER_TURN
        {
            return None;
        }
        let remaining = MAX_ASSISTANT_CHARS_PER_TURN.saturating_sub(self.assistant_chars);
        let text = text
            .chars()
            .take(remaining.min(MAX_EVENT_TEXT_CHARS))
            .collect::<String>();
        if text.is_empty() {
            return None;
        }
        self.assistant_events += 1;
        self.assistant_chars += text.chars().count();
        Some(text)
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

    fn finish(&mut self, completed: bool) {
        for listener_id in self.listener_ids.drain(..) {
            self.app.unlisten(listener_id);
        }
        // The same lock fences callback delivery, terminal acceptance and close.
        let mut state = self.state.lock();
        let (reasoning_batches, assistant_tail) = state.close_streams(completed);
        for text in reasoning_batches {
            self.events.emit_journal(
                JournalKind::Reasoning,
                &self.session_id,
                Some(&self.task_id),
                json!({ "text": text }),
            );
        }
        if let Some(text) = assistant_tail {
            self.events.emit_journal(
                JournalKind::AssistantDelta,
                &self.session_id,
                Some(&self.task_id),
                json!({ "text": text }),
            );
        }
    }
}

impl Drop for TurnEventRelayHandle {
    fn drop(&mut self) {
        self.finish(false);
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
    flush_reasoning_locked(&mut state, events, session_id, task_id, force);
}

fn flush_reasoning_locked(
    state: &mut TurnEventRelayState,
    events: &Arc<dyn ControllerTaskEventSink>,
    session_id: &str,
    task_id: &str,
    force: bool,
) {
    state.flush_scheduled = false;
    let batch = if state.stopped {
        None
    } else {
        take_reasoning_batch(state, force)
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

/// Delivery is synchronous: the production sink journals/enqueues and emits
/// distinct desktop-remote events; it never calls this chat relay. Keep the
/// lock until delivery finishes so close cannot overtake prepared output.
fn with_open_chat_relay(
    state: &Mutex<TurnEventRelayState>,
    deliver: impl FnOnce(&mut TurnEventRelayState),
) {
    let mut state = state.lock();
    if state.stopped || state.terminal.is_some() || state.generation.is_none() {
        return;
    }
    deliver(&mut state);
}

fn relay_chat_assistant(
    state: &Mutex<TurnEventRelayState>,
    events: &Arc<dyn ControllerTaskEventSink>,
    session_id: &str,
    task_id: &str,
    text: &str,
) {
    with_open_chat_relay(state, |state| {
        flush_reasoning_locked(state, events, session_id, task_id, true);
        if let Some(text) = state.push_assistant(text) {
            events.emit_journal(
                JournalKind::AssistantDelta,
                session_id,
                Some(task_id),
                json!({ "text": text }),
            );
        }
    });
}

fn relay_chat_tool(
    state: &Mutex<TurnEventRelayState>,
    events: &Arc<dyn ControllerTaskEventSink>,
    session_id: &str,
    task_id: &str,
    payload: Value,
) {
    with_open_chat_relay(state, |state| {
        flush_reasoning_locked(state, events, session_id, task_id, true);
        events.emit_journal(JournalKind::Tool, session_id, Some(task_id), payload);
    });
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

pub(super) fn remote_engine_error_code(error: &anyhow::Error) -> &'static str {
    if error
        .chain()
        .any(|cause| cause.to_string() == "model_credential_unavailable")
    {
        "model_credential_unavailable"
    } else if error
        .chain()
        .any(|cause| cause.to_string() == "model_runtime_unavailable")
    {
        "model_runtime_unavailable"
    } else {
        "task_engine_unavailable"
    }
}

fn ingest_published_attachments(
    session_root: &std::path::Path,
    attachments: Vec<PublishedAttachment>,
) -> std::result::Result<Vec<crate::features::files::file_ingest::IngestResult>, String> {
    let mut result = Vec::with_capacity(attachments.len());
    for attachment in attachments {
        result.push(
            crate::features::files::file_ingest::ingest_staged_attachment(
                session_root,
                &attachment,
            )?,
        );
    }
    Ok(result)
}

#[async_trait]
impl RemoteChatEnginePort for EnginePoolChatEngine {
    fn reserve_remote_turn(
        &self,
        session_id: &str,
    ) -> std::result::Result<Option<RemoteChatReservation>, String> {
        reserve_remote_chat_turn(session_id, || {
            self.pool
                .reserve_turn(session_id)
                .map_err(|_| "task_session_busy".to_owned())
        })
        .map(Some)
    }

    async fn send_user_message(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
        _relay: Option<&TurnEventRelayHandle>,
        owner: Arc<RemoteTurnOwner>,
        reserved: &mut Option<RemoteChatReservation>,
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
            .send_remote_user_message(
                session_id,
                content,
                skill_ids,
                model_override,
                owner,
                reserved
                    .as_mut()
                    .and_then(RemoteChatReservation::take_turn)
                    .ok_or_else(|| "task_turn_reservation_missing".to_owned())?,
            )
            .await
            .map_err(|error| remote_engine_error_code(&error).to_owned())
    }

    async fn send_user_message_with_attachments(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
        attachments: Vec<PublishedAttachment>,
        relay: Option<&TurnEventRelayHandle>,
        owner: Arc<RemoteTurnOwner>,
        reserved: &mut Option<RemoteChatReservation>,
    ) -> std::result::Result<(), String> {
        if attachments.is_empty() {
            return self
                .send_user_message(
                    session_id,
                    task_id,
                    model_source,
                    lease,
                    content,
                    skill_ids,
                    relay,
                    owner,
                    reserved,
                )
                .await;
        }
        let root = self
            .pool
            .session_state_root(session_id)
            .map_err(|_| "task_session_not_found".to_owned())?;
        // Protect input preparation and submission. The outer turn port also
        // retains these owners through terminal wait and cancellation teardown.
        let _attachment_read_owners: Vec<_> = attachments
            .iter()
            .filter_map(|attachment| attachment.read_owner.clone())
            .collect();
        let ingested =
            tokio::task::spawn_blocking(move || ingest_published_attachments(&root, attachments))
                .await
                .map_err(|_| "task_attachment_unavailable".to_owned())??;
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
            .send_remote_user_message_with_attachments(
                session_id,
                content,
                ingested,
                skill_ids,
                model_override,
                owner,
                reserved
                    .as_mut()
                    .and_then(RemoteChatReservation::take_turn)
                    .ok_or_else(|| "task_turn_reservation_missing".to_owned())?,
            )
            .await
            .map_err(|error| remote_engine_error_code(&error).to_owned())
    }

    fn is_turn_active(&self, session_id: &str, generation: Option<u64>) -> bool {
        generation.is_some_and(|generation| {
            self.pool
                .is_turn_active_for_generation(session_id, generation)
        })
    }

    fn terminal_result(
        &self,
        _session_id: &str,
        relay: Option<&TurnEventRelayHandle>,
    ) -> Option<std::result::Result<(), String>> {
        match relay {
            Some(relay) => relay.state.lock().terminal.clone(),
            None => Some(Err("task_turn_terminal_missing".to_owned())),
        }
    }

    async fn cancel_turn(&self, session_id: &str, generation: Option<u64>) {
        let Some(generation) = generation else { return };
        let _ = self.pool.cancel_remote_turn(session_id, generation).await;
        // Wait only for this fixed generation, never a newer local reservation.
        let started = Instant::now();
        while self
            .pool
            .is_turn_active_for_generation(session_id, generation)
            && started.elapsed() < BUSY_GRACE
        {
            tokio::time::sleep(POLL_INTERVAL).await;
        }
    }

    async fn release_task_model(&self, session_id: &str, owner: &Arc<RemoteTurnOwner>) {
        self.pool.evict_remote_owner(session_id, owner).await;
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
        let assistant_events = Arc::clone(&events);
        let assistant_session_id = session_id.to_owned();
        let assistant_task_id = task_id.to_owned();
        let assistant_listener = self.app.listen("chat:delta", move |event| {
            let Ok(payload) = serde_json::from_str::<Value>(event.payload()) else {
                return;
            };
            if payload.get("session_id").and_then(Value::as_str)
                != Some(assistant_session_id.as_str())
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
            // Already-classified reasoning is delivered first. An ambiguous
            // block waits for its own boundary rather than leaking a prefix
            // merely to preserve cross-channel arrival order.
            relay_chat_assistant(
                &assistant_state,
                &assistant_events,
                &assistant_session_id,
                &assistant_task_id,
                text,
            );
        });

        let tool_start_state = Arc::clone(&state);
        let tool_start_events = Arc::clone(&events);
        let tool_start_session_id = session_id.to_owned();
        let tool_start_task_id = task_id.to_owned();
        let tool_start_listener = self.app.listen("chat:tool_start", move |event| {
            let Ok(payload) = serde_json::from_str::<Value>(event.payload()) else {
                return;
            };
            if payload.get("session_id").and_then(Value::as_str)
                != Some(tool_start_session_id.as_str())
            {
                return;
            }
            let Some(tool_id) = payload.get("id").and_then(Value::as_str) else {
                return;
            };
            let Some(tool_name) = payload.get("name").and_then(Value::as_str) else {
                return;
            };
            // Never project `args`: tool arguments can contain credentials,
            // signed URLs, or B-local host paths.
            relay_chat_tool(
                &tool_start_state,
                &tool_start_events,
                &tool_start_session_id,
                &tool_start_task_id,
                json!({
                    "tool_id": tool_id,
                    "tool_name": tool_name,
                    "tool_state": "running",
                }),
            );
        });

        let tool_end_state = Arc::clone(&state);
        let tool_end_events = Arc::clone(&events);
        let tool_end_session_id = session_id.to_owned();
        let tool_end_task_id = task_id.to_owned();
        let tool_end_listener = self.app.listen("chat:tool_end", move |event| {
            let Ok(payload) = serde_json::from_str::<Value>(event.payload()) else {
                return;
            };
            if payload.get("session_id").and_then(Value::as_str)
                != Some(tool_end_session_id.as_str())
            {
                return;
            }
            let Some(tool_id) = payload.get("id").and_then(Value::as_str) else {
                return;
            };
            let Some(tool_name) = payload.get("name").and_then(Value::as_str) else {
                return;
            };
            let success = payload
                .get("success")
                .and_then(Value::as_bool)
                .unwrap_or(false);
            let output = payload
                .get("output")
                .and_then(Value::as_str)
                .map(sanitize_text)
                .filter(|text| !text.is_empty());
            relay_chat_tool(
                &tool_end_state,
                &tool_end_events,
                &tool_end_session_id,
                &tool_end_task_id,
                json!({
                    "tool_id": tool_id,
                    "tool_name": tool_name,
                    "tool_state": if success { "completed" } else { "failed" },
                    "text": output,
                }),
            );
        });

        let terminal_state = Arc::clone(&state);
        let terminal_session_id = session_id.to_owned();
        let terminal_listener = self.app.listen("chat:done", move |event| {
            if let Ok(payload) = serde_json::from_str::<Value>(event.payload()) {
                terminal_state
                    .lock()
                    .accept_terminal(&terminal_session_id, &payload);
            }
        });

        Some(TurnEventRelayHandle {
            app: self.app.clone(),
            listener_ids: vec![
                reasoning_listener,
                assistant_listener,
                tool_start_listener,
                tool_end_listener,
                terminal_listener,
            ],
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
        relay: Option<&TurnEventRelayHandle>,
        generation: Option<u64>,
    ) -> std::result::Result<(), String> {
        let mut inactive_since = None;
        loop {
            if cancel.is_cancelled() {
                self.engine.cancel_turn(session_id, generation).await;
                return Err(TASK_CANCELLED_CODE.to_owned());
            }
            if let Some(result) = self.engine.terminal_result(session_id, relay) {
                return result;
            }
            if self.engine.is_turn_active(session_id, generation) {
                inactive_since = None;
            } else if inactive_since.get_or_insert_with(Instant::now).elapsed() >= BUSY_GRACE {
                return Err("task_turn_terminal_missing".to_owned());
            }
            tokio::select! {
                _ = cancel.cancelled() => {
                    self.engine.cancel_turn(session_id, generation).await;
                    return Err(TASK_CANCELLED_CODE.to_owned());
                }
                _ = tokio::time::sleep(POLL_INTERVAL) => {}
            }
        }
    }
}

impl EnginePoolChatTurnPort {
    async fn run_chat_turn_with_attachments_budget(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
        budget: TurnBudget,
    ) -> std::result::Result<Option<String>, String> {
        if cancel.is_cancelled() {
            return Err(TASK_CANCELLED_CODE.to_owned());
        }
        // Submission only queues the engine turn. Receipt revocation during
        // disconnect must not release paths still used by later tool reads.
        let _attachment_read_owners: Vec<_> = attachments
            .iter()
            .filter_map(|attachment| attachment.read_owner.clone())
            .collect();
        let owner = Arc::new(RemoteTurnOwner::default());
        let mut reserved = self.engine.reserve_remote_turn(session_id)?;
        let generation = reserved.as_ref().map(|reserved| reserved.generation);
        let mut turn_relay = self.engine.start_turn_relay(session_id, task_id, events);
        if let Some(relay) = turn_relay.as_mut() {
            if let Some(reserved) = reserved.as_ref() {
                if let Some(turn) = reserved.turn.as_ref() {
                    relay.arm(turn.generation());
                }
            }
        }
        let result = budget
            .run(&cancel, async {
                tokio::select! {
                    biased;
                    _ = cancel.cancelled() => {
                        return Err(TASK_CANCELLED_CODE.to_owned());
                    }
                    result = self.engine.send_user_message_with_attachments(
                        session_id,
                        task_id,
                        model_source,
                        lease,
                        content,
                        skill_ids,
                        attachments,
                        turn_relay.as_ref(),
                        owner.clone(),
                        &mut reserved,
                    ) => result?,
                }
                self.wait_for_terminal(session_id, &cancel, turn_relay.as_ref(), generation)
                    .await?;
                Ok(())
            })
            .await;
        if matches!(
            result.as_ref().err().map(String::as_str),
            Some(TASK_CANCELLED_CODE | TURN_TIMEOUT_CODE)
        ) {
            let _ = tokio::time::timeout(
                CANCEL_SETTLE_BUDGET,
                self.engine.cancel_turn(session_id, generation),
            )
            .await;
        }
        // Only a successful, accepted terminal can classify the final block.
        // Cancellation, failure and Drop mask pending text instead.
        // Assistant content is journaled from real `chat:delta` callbacks,
        // never synthesized as one final response here.
        if let Some(relay) = turn_relay.as_mut() {
            relay.finish(result.is_ok());
        }
        // All remote model sources own a private skills root. Retire only this
        // attempt's entry before releasing credentials; never rewrite shared B
        // skills here, especially when a replacement already owns the session.
        let engine = self.engine.clone();
        let session_id = session_id.to_owned();
        crate::features::remote_control::controller::turn_budget::settle_owned(async move {
            engine.release_task_model(&session_id, &owner).await;
        })
        .await;
        if result.is_ok() {
            budget.checkpoint(&cancel)?;
        }
        result.map(|()| None)
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
        skill_ids: Vec<String>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<Option<String>, String> {
        self.run_chat_turn_with_attachments(
            session_id,
            task_id,
            model_source,
            lease,
            content,
            skill_ids,
            Vec::new(),
            events,
            cancel,
        )
        .await
    }

    async fn run_chat_turn_with_attachments(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
        attachments: Vec<PublishedAttachment>,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> std::result::Result<Option<String>, String> {
        let budget = TurnBudget::current_or_new()?;
        self.run_chat_turn_with_attachments_budget(
            session_id,
            task_id,
            model_source,
            lease,
            content,
            skill_ids,
            attachments,
            events,
            cancel,
            budget,
        )
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::assistant::engine::TurnLifecycle;
    use crate::features::assistant::engine_pool::prepare_selected_runtime_model;
    use crate::features::assistant::platform::bridge::Pinvou3Bridge;
    use crate::features::assistant::runtime_model::{
        ModelCredentialMode, PreparedRuntimeModel, RuntimeModelProvider, RuntimeModelRequest,
    };
    use crate::platform::credential_store::{
        CredentialEditAction, CredentialReference, CredentialState, CredentialStore,
        MemoryCredentialStore,
    };
    use crate::platform::prefs::{ImageCapabilityOverride, ModelPreset, SavedModel};
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    #[test]
    fn remote_reservation_takes_turn_gate_first_and_consumes_it_once() {
        let lifecycle = Arc::new(TurnLifecycle::default());
        let sid = format!("remote-reserve-first-{}", uuid::Uuid::new_v4());
        let reserve = || {
            lifecycle
                .reserve()
                .map_err(|_| "task_session_busy".to_owned())
        };
        let local = lifecycle.reserve().unwrap();
        assert!(reserve_remote_chat_turn(&sid, reserve).is_err());
        assert!(local.ensure_active().is_ok());
        drop(local);

        let mut remote = reserve_remote_chat_turn(&sid, reserve).unwrap();
        let generation = remote.generation;
        let turn = remote.take_turn().unwrap();
        assert_eq!(turn.generation(), generation);
        assert!(remote.take_turn().is_none());
        assert_eq!(
            remote.generation, generation,
            "cancel identity survives take_turn"
        );
        drop(turn);
        let successor = lifecycle.reserve().unwrap();
        assert_ne!(successor.generation(), remote.generation);
        drop(remote);
        assert!(
            successor.ensure_active().is_ok(),
            "old reservation Drop cannot retire its successor"
        );
    }

    struct EnvRestore(Vec<(&'static str, Option<std::ffi::OsString>)>);

    impl EnvRestore {
        fn capture(names: &[&'static str]) -> Self {
            Self(
                names
                    .iter()
                    .map(|name| (*name, std::env::var_os(name)))
                    .collect(),
            )
        }
    }

    impl Drop for EnvRestore {
        fn drop(&mut self) {
            for (name, value) in self.0.drain(..) {
                match value {
                    // SAFETY: callers hold platform::paths::tests::ENV_LOCK while mutating process env.
                    Some(value) => unsafe { std::env::set_var(name, value) },
                    // SAFETY: callers hold platform::paths::tests::ENV_LOCK while mutating process env.
                    None => unsafe { std::env::remove_var(name) },
                }
            }
        }
    }

    fn isolated_remote_bridge() -> (Pinvou3Bridge, std::path::PathBuf, EnvRestore) {
        let restore = EnvRestore::capture(&[
            "PINVOU3_HOME",
            "DEEPSEEK_MODEL",
            "DEEPSEEK_PROVIDER",
            "DEEPSEEK_BASE_URL",
            "DEEPSEEK_API_KEY",
            "DEEPSEEK_MAX_OUTPUT_TOKENS",
            "PINVOU3_SESSION_ARTIFACTS",
        ]);
        let home = std::env::temp_dir().join(format!(
            "pinvou-remote-runtime-{}-{}",
            std::process::id(),
            crate::platform::paths::tests::unique_suffix()
        ));
        // SAFETY: the caller holds platform::paths::tests::ENV_LOCK.
        unsafe { std::env::set_var("PINVOU3_HOME", &home) };
        for name in [
            "DEEPSEEK_MODEL",
            "DEEPSEEK_PROVIDER",
            "DEEPSEEK_BASE_URL",
            "DEEPSEEK_API_KEY",
        ] {
            // SAFETY: the caller holds platform::paths::tests::ENV_LOCK.
            unsafe { std::env::remove_var(name) };
        }
        let bridge = Pinvou3Bridge::boot().expect("boot isolated remote bridge");
        (bridge, home, restore)
    }

    fn runtime_model(id: &str, wire_name: &str) -> SavedModel {
        SavedModel {
            id: id.to_string(),
            name: id.to_string(),
            alias: None,
            preset: ModelPreset::OpenaiCompatible,
            context_window_tokens: None,
            max_output_tokens: None,
            reasoning_effort: None,
            model: wire_name.to_string(),
            base_url: "https://example.invalid/v1".to_string(),
            provider_kind: None,
            vendor: None,
            endpoint_mode: None,
            image_capability_override: ImageCapabilityOverride::default(),
            vision_model_id: None,
            api_key: String::new(),
            credential_ref: None,
            credential_state: CredentialState::Missing,
            has_secret: false,
            credential_action: None::<CredentialEditAction>,
        }
    }

    struct PipelineRuntimeProvider {
        calls: AtomicUsize,
        fail: AtomicBool,
    }

    #[async_trait]
    impl RuntimeModelProvider for PipelineRuntimeProvider {
        fn credential_mode(
            &self,
            _model: &SavedModel,
            _user_api_key_required: bool,
        ) -> ModelCredentialMode {
            ModelCredentialMode::UserManaged
        }

        async fn prepare(
            &self,
            request: RuntimeModelRequest,
        ) -> anyhow::Result<PreparedRuntimeModel> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            if self.fail.load(Ordering::SeqCst) {
                anyhow::bail!("private provider preparation detail");
            }
            Ok(PreparedRuntimeModel::unchanged(request.model))
        }
    }

    struct SnapshotBoundaryEngine {
        provider: Arc<PipelineRuntimeProvider>,
        credential_store: Arc<MemoryCredentialStore>,
        credential_ref: CredentialReference,
        bridge: Pinvou3Bridge,
        selected_model: Mutex<SavedModel>,
        mutate_after_freeze: Mutex<Option<(String, String)>>,
        configured_routes: Mutex<Vec<(String, String)>>,
        lifecycle: Arc<TurnLifecycle>,
        active: AtomicBool,
    }

    #[async_trait]
    impl RemoteChatEnginePort for SnapshotBoundaryEngine {
        async fn send_user_message(
            &self,
            session_id: &str,
            _task_id: &str,
            _model_source: &str,
            _lease: &LeaseContext,
            _content: String,
            _skill_ids: Vec<String>,
            _relay: Option<&TurnEventRelayHandle>,
            _owner: Arc<RemoteTurnOwner>,
            _reserved: &mut Option<RemoteChatReservation>,
        ) -> std::result::Result<(), String> {
            let reservation = self
                .lifecycle
                .reserve()
                .map_err(|_| "task_engine_unavailable".to_owned())?;
            let selected = self
                .selected_model
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .clone();
            let mut bridge = self.bridge.clone();
            bridge.session_model = Some(selected.clone());
            prepare_selected_runtime_model(
                self.provider.as_ref(),
                self.credential_store.as_ref(),
                &mut bridge,
                RuntimeModelRequest {
                    session_id: session_id.to_owned(),
                    model: selected,
                    scheduled_unattended: false,
                },
                None,
                false,
                true,
            )
            .await
            .map_err(|error| remote_engine_error_code(&error).to_owned())?;

            if let Some((next_key, next_model)) = self
                .mutate_after_freeze
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .take()
            {
                self.credential_store
                    .set(&self.credential_ref, &next_key)
                    .expect("rotate memory credential after the snapshot");
                self.selected_model
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner())
                    .model = next_model;
            }

            let config = bridge.build_dt_config();
            self.configured_routes
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .push((
                    config.api_key.unwrap_or_default(),
                    config.default_text_model.unwrap_or_default(),
                ));
            assert!(reservation.ensure_active().is_ok());
            drop(reservation);
            self.active.store(true, Ordering::SeqCst);
            Ok(())
        }

        fn is_turn_active(&self, _session_id: &str, _generation: Option<u64>) -> bool {
            self.active.swap(false, Ordering::SeqCst)
        }

        fn terminal_result(
            &self,
            _session_id: &str,
            _relay: Option<&TurnEventRelayHandle>,
        ) -> Option<std::result::Result<(), String>> {
            (!self.active.load(Ordering::SeqCst)).then_some(Ok(()))
        }

        async fn cancel_turn(&self, _session_id: &str, _generation: Option<u64>) {}

        async fn release_task_model(&self, _session_id: &str, _owner: &Arc<RemoteTurnOwner>) {}
    }

    fn remote_task_payload(task_id: &str, client_request_id: &str) -> Value {
        let input = json!({
            "text": "run the report",
            "attachment_ids": [],
            "skill_ids": [],
        });
        let input_digest = task_input_digest_for_value(&input).expect("task digest");
        json!({
            "v": 1,
            "type": "task_submit",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "1",
            "relay_epoch": "relay-0001",
            "remote_control_generation": "generation-0001",
            "request_id": client_request_id,
            "task_id": task_id,
            "client_request_id": client_request_id,
            "session_id": "session-0001",
            "capability_revision": "7",
            "model_source": "controlled_device",
            "mode": "chat",
            "input_digest": input_digest,
            "input": input,
        })
    }

    #[test]
    fn completed_b_upload_is_ingested_as_content_not_only_a_path() {
        use sha2::{Digest, Sha256};

        let root = tempfile::tempdir().unwrap();
        let staged = root.path().join("remote_uploads").join("ctr-up-0001");
        std::fs::create_dir_all(&staged).unwrap();
        let path = staged.join("report.txt");
        let body = b"attachment evidence reaches the model";
        let publication = crate::platform::filesystem::publish_private_read_path(
            crate::platform::filesystem::open_private_file_directory(&staged).unwrap(),
            std::ffi::OsStr::new("report.txt"),
            body,
        );
        if !crate::platform::filesystem::supports_private_read_path_publication() {
            assert_eq!(
                publication.unwrap_err().kind(),
                std::io::ErrorKind::Unsupported
            );
            assert!(!path.exists());
            return;
        }
        let owner = Arc::new(publication.unwrap());
        let digest = Sha256::digest(body)
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect();
        let descriptor = PublishedAttachment {
            handle_id: "ctr-up-0001".to_owned(),
            path: owner.path().to_owned(),
            display_name: "report.txt".to_owned(),
            mime_type: "text/plain".to_owned(),
            bytes: body.len() as u64,
            sha256: digest,
            read_owner: Some(owner),
        };
        let ingested = ingest_published_attachments(root.path(), vec![descriptor.clone()]).unwrap();
        assert_eq!(ingested.len(), 1);
        assert_eq!(ingested[0].basename, "report.txt");
        assert!(
            ingested[0]
                .markdown
                .as_deref()
                .unwrap()
                .contains("attachment evidence")
        );

        let mut tampered = descriptor.clone();
        tampered.sha256 = "0".repeat(64);
        assert_eq!(
            ingest_published_attachments(root.path(), vec![tampered]).unwrap_err(),
            "task_attachment_invalid"
        );
        let outside = root.path().join("outside.txt");
        std::fs::write(&outside, body).unwrap();
        let mut escaped = descriptor;
        escaped.path = outside;
        assert_eq!(
            ingest_published_attachments(root.path(), vec![escaped]).unwrap_err(),
            "task_attachment_invalid"
        );
    }

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
        protected_attachment: Option<std::path::PathBuf>,
        fail_submission: bool,
        cancel_after_submission: Option<CancellationToken>,
        lease_checks: AtomicUsize,
        terminal_outcome: Option<std::result::Result<(), String>>,
        missing_terminal: bool,
        real_lifecycle: Option<Arc<TurnLifecycle>>,
        private_skills: Mutex<Option<Arc<super::super::skill_materialization::RemoteTurnSkills>>>,
        eviction_started: Option<Arc<tokio::sync::Notify>>,
        eviction_release: Option<Arc<tokio::sync::Notify>>,
    }

    impl FakeEngine {
        fn assert_attachment_lease(&self) {
            if let Some(path) = self.protected_attachment.as_ref() {
                assert_eq!(std::fs::read(path).unwrap(), b"delayed tool input");
                assert!(std::fs::write(path, b"replacement").is_err());
                self.lease_checks.fetch_add(1, Ordering::SeqCst);
            }
        }

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
        fn reserve_remote_turn(
            &self,
            session_id: &str,
        ) -> std::result::Result<Option<RemoteChatReservation>, String> {
            self.real_lifecycle
                .as_ref()
                .map(|lifecycle| {
                    reserve_remote_chat_turn(session_id, || {
                        lifecycle
                            .reserve()
                            .map_err(|_| "task_session_busy".to_owned())
                    })
                })
                .transpose()
        }
        async fn send_user_message(
            &self,
            _session_id: &str,
            _task_id: &str,
            model_source: &str,
            _lease: &LeaseContext,
            content: String,
            _skill_ids: Vec<String>,
            _relay: Option<&TurnEventRelayHandle>,
            _owner: Arc<RemoteTurnOwner>,
            reserved: &mut Option<RemoteChatReservation>,
        ) -> std::result::Result<(), String> {
            if let Some(reserved) = reserved.as_mut() {
                let skills =
                    super::super::skill_materialization::materialize_plain_remote_turn_skills(
                        _session_id,
                        &[],
                        _owner,
                    )?;
                *self.private_skills.lock().unwrap() = Some(Arc::new(skills));
                drop(reserved.take_turn().expect("single reserved turn"));
            }
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

        fn is_turn_active(&self, _session_id: &str, generation: Option<u64>) -> bool {
            self.assert_attachment_lease();
            if let Some(lifecycle) = &self.real_lifecycle {
                self.flips.fetch_add(1, Ordering::SeqCst);
                return generation.is_some_and(|generation| {
                    !lifecycle.is_reserve_gate_open_for(Some(generation))
                });
            }
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

        fn terminal_result(
            &self,
            _session_id: &str,
            _relay: Option<&TurnEventRelayHandle>,
        ) -> Option<std::result::Result<(), String>> {
            (!self.missing_terminal
                && self.completes_after_first_poll
                && self.flips.load(Ordering::SeqCst) > 0)
                .then(|| self.terminal_outcome.clone().unwrap_or(Ok(())))
        }

        async fn cancel_turn(&self, session_id: &str, _generation: Option<u64>) {
            self.assert_attachment_lease();
            self.cancelled.lock().unwrap().push(session_id.to_owned());
        }

        async fn release_task_model(&self, session_id: &str, _owner: &Arc<RemoteTurnOwner>) {
            self.assert_attachment_lease();
            if let Some(started) = &self.eviction_started {
                started.notify_one();
            }
            if let Some(release) = &self.eviction_release {
                release.notified().await;
            }
            self.released.lock().unwrap().push(session_id.to_owned());
            self.private_skills.lock().unwrap().take();
        }

        async fn send_user_message_with_attachments(
            &self,
            session_id: &str,
            task_id: &str,
            model_source: &str,
            lease: &LeaseContext,
            content: String,
            skill_ids: Vec<String>,
            attachments: Vec<PublishedAttachment>,
            relay: Option<&TurnEventRelayHandle>,
            owner: Arc<RemoteTurnOwner>,
            reserved: &mut Option<RemoteChatReservation>,
        ) -> std::result::Result<(), String> {
            if self.protected_attachment.is_none() && !attachments.is_empty() {
                return Err("task_attachments_unavailable".to_owned());
            }
            // Observe ownership at submission and at exact-owner teardown.
            // Failed submission has no busy polling or shared-Skills restore.
            self.assert_attachment_lease();
            let result = self
                .send_user_message(
                    session_id,
                    task_id,
                    model_source,
                    lease,
                    content,
                    skill_ids,
                    relay,
                    owner,
                    reserved,
                )
                .await;
            // Simulate all receipt/submission owners disappearing on return.
            for attachment in &attachments {
                if let Some(owner) = attachment.read_owner.as_ref() {
                    owner.request_cleanup();
                }
            }
            drop(attachments);
            if let Some(cancel) = self.cancel_after_submission.as_ref() {
                cancel.cancel();
            }
            if self.fail_submission {
                return Err("task_engine_unavailable".to_owned());
            }
            result
        }
    }

    #[test]
    fn attachment_lease_survives_receipt_revocation_until_terminal_and_teardown() {
        use sha2::{Digest as _, Sha256};
        for outcome in [
            "completed",
            "submission_failed",
            "cancel_wait",
            "cancel_before_send",
        ] {
            let temp = tempfile::tempdir().unwrap();
            let folder = temp.path().join("owned");
            let publication = crate::platform::filesystem::publish_private_read_path(
                crate::platform::filesystem::open_private_file_directory(&folder).unwrap(),
                std::ffi::OsStr::new("report.txt"),
                b"delayed tool input",
            );
            if !crate::platform::filesystem::supports_private_read_path_publication() {
                assert_eq!(
                    publication.unwrap_err().kind(),
                    std::io::ErrorKind::Unsupported
                );
                assert!(!folder.join("report.txt").exists());
                return;
            }
            let owner = Arc::new(publication.unwrap());
            let path = owner.path().to_owned();
            owner.request_cleanup();
            let descriptor = PublishedAttachment {
                handle_id: "ctr-up-0001".to_owned(),
                path: path.clone(),
                display_name: "report.txt".to_owned(),
                mime_type: "text/plain".to_owned(),
                bytes: 18,
                sha256: Sha256::digest(b"delayed tool input")
                    .iter()
                    .map(|byte| format!("{byte:02x}"))
                    .collect(),
                read_owner: Some(owner),
            };
            let cancel = CancellationToken::new();
            if outcome == "cancel_before_send" {
                cancel.cancel();
            }
            let fake = Arc::new(FakeEngine {
                completes_after_first_poll: true,
                protected_attachment: Some(path.clone()),
                fail_submission: outcome == "submission_failed",
                cancel_after_submission: (outcome == "cancel_wait").then(|| cancel.clone()),
                ..FakeEngine::default()
            });
            let port = EnginePoolChatTurnPort::new(fake.clone());
            let result = runtime().block_on(port.run_chat_turn_with_attachments(
                "session-0001",
                "task-0001",
                TASK_MODEL_SOURCE_TEMPORARY,
                &lease(),
                "read the attachment".to_owned(),
                Vec::new(),
                vec![descriptor],
                events(),
                cancel,
            ));
            match outcome {
                "completed" => assert_eq!(result.unwrap(), None),
                "submission_failed" => assert_eq!(result.unwrap_err(), "task_engine_unavailable"),
                _ => assert_eq!(result.unwrap_err(), TASK_CANCELLED_CODE),
            }
            if outcome == "cancel_before_send" {
                assert_eq!(fake.lease_checks.load(Ordering::SeqCst), 0);
                assert!(fake.sent.lock().unwrap().is_empty());
                assert!(fake.released.lock().unwrap().is_empty());
            } else {
                assert!(fake.lease_checks.load(Ordering::SeqCst) >= 2, "{outcome}");
                assert_eq!(
                    *fake.released.lock().unwrap(),
                    vec!["session-0001".to_owned()],
                    "{outcome}: attachment remains protected through exact-owner teardown"
                );
            }
            assert!(
                !path.exists(),
                "cleanup waits for the turn port's last owner"
            );
            assert!(!folder.exists());
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
    fn runtime_preparation_errors_are_bounded_before_relay() {
        let missing = anyhow::anyhow!("model_credential_unavailable")
            .context("failed to construct provider client");
        assert_eq!(
            super::remote_engine_error_code(&missing),
            "model_credential_unavailable"
        );

        let provider = anyhow::anyhow!("private provider detail")
            .context("model_runtime_unavailable")
            .context("runtime preparation failed");
        assert_eq!(
            super::remote_engine_error_code(&provider),
            "model_runtime_unavailable"
        );
        assert!(!super::remote_engine_error_code(&provider).contains("private"));
        assert_eq!(
            super::remote_engine_error_code(&anyhow::anyhow!("session missing")),
            "task_engine_unavailable"
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn remote_task_pipeline_freezes_runtime_snapshot_and_releases_both_turn_gates() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let (bridge, home, _env) = isolated_remote_bridge();
        let provider = Arc::new(PipelineRuntimeProvider {
            calls: AtomicUsize::new(0),
            fail: AtomicBool::new(false),
        });
        let credential_store = Arc::new(MemoryCredentialStore::default());
        let credential_ref = CredentialReference::for_model("controlled");
        let mut selected = runtime_model("controlled", "wire-v1");
        selected.credential_ref = Some(credential_ref.clone());
        let engine = Arc::new(SnapshotBoundaryEngine {
            provider: provider.clone(),
            credential_store: credential_store.clone(),
            credential_ref: credential_ref.clone(),
            bridge,
            selected_model: Mutex::new(selected),
            mutate_after_freeze: Mutex::new(None),
            configured_routes: Mutex::new(Vec::new()),
            lifecycle: Arc::new(TurnLifecycle::default()),
            active: AtomicBool::new(false),
        });
        let turn_port = Arc::new(EnginePoolChatTurnPort::new(engine.clone()));
        let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        service.register_executor(TASK_MODE_CHAT, Arc::new(ChatTurnExecutor::new(turn_port)));
        let lease = lease();

        let first_payload = remote_task_payload("task-0001", "client-0001");
        let first = match service.submit(&lease, &first_payload, 1_000).unwrap() {
            AdmissionOutcome::Admitted(ticket) => ticket,
            other => panic!("expected first admission, got {other:?}"),
        };
        let missing = service
            .dispatch(first, events(), CancellationToken::new(), 1_100)
            .await
            .unwrap();
        assert_eq!(missing.state, RemoteTaskState::Failed);
        assert_eq!(
            missing.terminal_reason.as_deref(),
            Some("model_credential_unavailable")
        );
        assert!(!engine.lifecycle.is_active());
        assert_eq!(service.ledger().live_task_count(), 0);
        assert_eq!(provider.calls.load(Ordering::SeqCst), 1);

        match service.submit(&lease, &first_payload, 1_200).unwrap() {
            AdmissionOutcome::Replay(record) => {
                assert_eq!(record.state, RemoteTaskState::Failed)
            }
            other => panic!("expected durable replay, got {other:?}"),
        }
        assert_eq!(provider.calls.load(Ordering::SeqCst), 1);

        credential_store
            .set(&credential_ref, "frozen-key")
            .expect("seed memory credential");
        *engine
            .mutate_after_freeze
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner()) =
            Some(("rotated-key".to_owned(), "wire-v2".to_owned()));
        let second_payload = remote_task_payload("task-0002", "client-0002");
        let second = match service.submit(&lease, &second_payload, 1_300).unwrap() {
            AdmissionOutcome::Admitted(ticket) => ticket,
            other => panic!("expected second admission, got {other:?}"),
        };
        let succeeded = service
            .dispatch(second, events(), CancellationToken::new(), 1_400)
            .await
            .unwrap();
        assert_eq!(succeeded.state, RemoteTaskState::Succeeded);
        assert_eq!(
            engine
                .configured_routes
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .as_slice(),
            &[("frozen-key".to_owned(), "wire-v1".to_owned())]
        );
        assert_eq!(
            credential_store.get(&credential_ref).unwrap().as_deref(),
            Some("rotated-key")
        );
        assert_eq!(provider.calls.load(Ordering::SeqCst), 2);
        assert!(!engine.lifecycle.is_active());

        provider.fail.store(true, Ordering::SeqCst);
        let third_payload = remote_task_payload("task-0003", "client-0003");
        let third = match service.submit(&lease, &third_payload, 1_500).unwrap() {
            AdmissionOutcome::Admitted(ticket) => ticket,
            other => panic!("expected third admission, got {other:?}"),
        };
        let provider_failed = service
            .dispatch(third, events(), CancellationToken::new(), 1_600)
            .await
            .unwrap();
        assert_eq!(provider_failed.state, RemoteTaskState::Failed);
        assert_eq!(
            provider_failed.terminal_reason.as_deref(),
            Some("model_runtime_unavailable")
        );
        assert_eq!(provider.calls.load(Ordering::SeqCst), 3);
        assert!(!engine.lifecycle.is_active());

        provider.fail.store(false, Ordering::SeqCst);
        let fourth_payload = remote_task_payload("task-0004", "client-0004");
        let fourth = match service.submit(&lease, &fourth_payload, 1_700).unwrap() {
            AdmissionOutcome::Admitted(ticket) => ticket,
            other => panic!("expected fourth admission, got {other:?}"),
        };
        let recovered = service
            .dispatch(fourth, events(), CancellationToken::new(), 1_800)
            .await
            .unwrap();
        assert_eq!(recovered.state, RemoteTaskState::Succeeded);
        assert_eq!(provider.calls.load(Ordering::SeqCst), 4);
        assert_eq!(
            engine
                .configured_routes
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .last(),
            Some(&("rotated-key".to_owned(), "wire-v2".to_owned()))
        );
        assert!(!engine.lifecycle.is_active());
        assert_eq!(service.ledger().live_task_count(), 0);
        let _ = std::fs::remove_dir_all(home);
    }

    #[test]
    fn turn_is_submitted_and_completion_waits_for_authoritative_terminal() {
        let fake = FakeEngine::immediate();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        let reply = runtime()
            .block_on(port.run_chat_turn(
                "session-0001",
                "task-0001",
                TASK_MODEL_SOURCE_CONTROLLED,
                &lease(),
                "hello remote".into(),
                Vec::new(),
                events(),
                CancellationToken::new(),
            ))
            .unwrap();
        assert_eq!(fake.sent.lock().unwrap().as_slice(), ["hello remote"]);
        assert_eq!(reply, None, "the fake engine does not install a turn relay");
    }

    #[tokio::test]
    async fn chat_terminal_outcomes_reach_the_task_ledger_without_false_success() {
        for (outcome, expected) in [
            (Ok(()), RemoteTaskState::Succeeded),
            (Err("task_turn_failed".to_owned()), RemoteTaskState::Failed),
            (
                Err(TASK_CANCELLED_CODE.to_owned()),
                RemoteTaskState::Cancelled,
            ),
        ] {
            let engine = Arc::new(FakeEngine {
                completes_after_first_poll: true,
                terminal_outcome: Some(outcome.clone()),
                ..FakeEngine::default()
            });
            let service = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
            service.register_executor(
                TASK_MODE_CHAT,
                Arc::new(ChatTurnExecutor::new(Arc::new(
                    EnginePoolChatTurnPort::new(engine),
                ))),
            );
            let payload = remote_task_payload("task-0001", "client-0001");
            let AdmissionOutcome::Admitted(ticket) =
                service.submit(&lease(), &payload, 1_000).unwrap()
            else {
                panic!("expected admission")
            };
            let record = tokio::time::timeout(
                Duration::from_secs(2),
                service.dispatch(ticket, events(), CancellationToken::new(), 1_100),
            )
            .await
            .unwrap()
            .unwrap();
            assert_eq!(record.state, expected);
            assert_eq!(
                record.terminal_reason,
                match expected {
                    RemoteTaskState::Failed => outcome.err(),
                    RemoteTaskState::Succeeded => Some("completed".to_owned()),
                    RemoteTaskState::Cancelled => Some("cancelled".to_owned()),
                    _ => unreachable!("test covers terminal states only"),
                }
            );
            assert_eq!(service.ledger().live_task_count(), 0);
        }
    }

    #[test]
    fn old_remote_cleanup_preserves_new_local_reservation_and_shared_skills() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let (_bridge, home, _env) = isolated_remote_bridge();
        runtime().block_on(async {
            for source in [TASK_MODEL_SOURCE_TEMPORARY, TASK_MODEL_SOURCE_CONTROLLED] {
                let sid = "remote-cleanup-replacement";
                let started = Arc::new(tokio::sync::Notify::new());
                let release = Arc::new(tokio::sync::Notify::new());
                let lifecycle = Arc::new(TurnLifecycle::default());
                let engine = Arc::new(FakeEngine {
                    completes_after_first_poll: true,
                    real_lifecycle: Some(lifecycle.clone()),
                    eviction_started: Some(started.clone()),
                    eviction_release: Some(release.clone()),
                    ..FakeEngine::default()
                });
                let old = engine.clone();
                let task = tokio::spawn(async move {
                    EnginePoolChatTurnPort::new(old)
                        .run_chat_turn(
                            sid,
                            "task-0001",
                            source,
                            &lease(),
                            "hello".to_owned(),
                            vec![],
                            events(),
                            CancellationToken::new(),
                        )
                        .await
                });
                tokio::time::timeout(Duration::from_secs(2), started.notified())
                    .await
                    .unwrap();
                let private = engine
                    .private_skills
                    .lock()
                    .unwrap()
                    .as_ref()
                    .unwrap()
                    .path()
                    .to_owned();
                assert!(private.starts_with(&home));
                assert!(private.is_dir());
                let successor = lifecycle.reserve().unwrap();
                super::super::skill_materialization::ensure_session_skills(
                    sid,
                    crate::features::marketplace::ConnectorScope::Plain,
                    None,
                );
                let shared = crate::platform::paths::session_skills_dir(sid);
                std::fs::write(shared.join("new-local-marker"), b"new local").unwrap();
                assert!(std::fs::read_dir(&private).unwrap().next().is_none());
                release.notify_one();
                assert!(
                    tokio::time::timeout(Duration::from_secs(2), task)
                        .await
                        .unwrap()
                        .unwrap()
                        .is_ok()
                );
                assert!(successor.ensure_active().is_ok());
                assert_eq!(
                    std::fs::read(shared.join("new-local-marker")).unwrap(),
                    b"new local"
                );
                assert!(!private.exists());
                assert_eq!(*engine.released.lock().unwrap(), vec![sid.to_owned()]);
            }
        });
    }

    #[tokio::test]
    async fn successor_activity_cannot_extend_old_remote_terminal_budget() {
        let lifecycle = Arc::new(TurnLifecycle::default());
        let old = lifecycle.reserve().unwrap();
        let generation = old.generation();
        drop(old);
        let successor = lifecycle.reserve().unwrap();
        let engine = Arc::new(FakeEngine {
            missing_terminal: true,
            real_lifecycle: Some(lifecycle),
            ..FakeEngine::default()
        });
        let port = EnginePoolChatTurnPort::new(engine);
        let result = tokio::time::timeout(
            BUSY_GRACE + Duration::from_secs(2),
            port.wait_for_terminal(
                "session-0001",
                &CancellationToken::new(),
                None,
                Some(generation),
            ),
        )
        .await
        .expect("new local activity must not renew old terminal budget");
        assert_eq!(result.unwrap_err(), "task_turn_terminal_missing");
        assert!(successor.ensure_active().is_ok());
    }

    #[test]
    fn inactive_engine_without_terminal_never_reports_success() {
        let fake = Arc::new(FakeEngine {
            completes_after_first_poll: true,
            missing_terminal: true,
            ..FakeEngine::default()
        });
        let port = EnginePoolChatTurnPort::new(fake);
        let result = runtime().block_on(async {
            tokio::time::timeout(
                Duration::from_secs(7),
                port.run_chat_turn(
                    "session-0001",
                    "task-0001",
                    TASK_MODEL_SOURCE_CONTROLLED,
                    &lease(),
                    "hello".to_owned(),
                    Vec::new(),
                    events(),
                    CancellationToken::new(),
                ),
            )
            .await
            .unwrap()
        });
        assert_eq!(result.unwrap_err(), "task_turn_terminal_missing");
    }

    #[test]
    fn chat_done_requires_exact_session_generation_and_sanitizes_failure() {
        for (status, error, cleanup, rejected, expected) in [
            ("Completed", Value::Null, false, false, Ok(())),
            (
                "Failed",
                json!("private provider credential"),
                false,
                false,
                Err("task_turn_failed".to_owned()),
            ),
            (
                "Interrupted",
                Value::Null,
                false,
                false,
                Err(TASK_CANCELLED_CODE.to_owned()),
            ),
            (
                "Completed",
                json!("provider 500"),
                false,
                false,
                Err("task_turn_failed".to_owned()),
            ),
            (
                "Completed",
                Value::Null,
                true,
                false,
                Err("task_turn_failed".to_owned()),
            ),
            (
                "Completed",
                Value::Null,
                false,
                true,
                Err("task_turn_failed".to_owned()),
            ),
            (
                "unknown",
                Value::Null,
                false,
                false,
                Err("task_turn_failed".to_owned()),
            ),
        ] {
            let mut state = TurnEventRelayState {
                generation: Some(7),
                ..Default::default()
            };
            let mut payload = json!({"session_id": "other", "generation": 7,
                "status": status, "error": error,
                "shell_cleanup_failed": cleanup, "operation_rejected": rejected});
            state.accept_terminal("session", &payload);
            assert!(state.terminal.is_none());
            payload["session_id"] = json!("session");
            payload["generation"] = json!(6);
            state.accept_terminal("session", &payload);
            assert!(state.terminal.is_none());
            payload["generation"] = json!(7);
            state.accept_terminal("session", &payload);
            assert_eq!(state.terminal, Some(expected.clone()));
            payload["status"] = json!("Completed");
            payload["error"] = Value::Null;
            state.accept_terminal("session", &payload);
            assert_eq!(
                state.terminal,
                Some(expected),
                "duplicate cannot rewrite failure"
            );
        }
        let mut unarmed = TurnEventRelayState::default();
        unarmed.accept_terminal(
            "session",
            &json!({"session_id":"session", "generation":null}),
        );
        assert!(unarmed.terminal.is_none());
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
                    Vec::new(),
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
                    Vec::new(),
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
                Vec::new(),
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

    #[tokio::test(start_paused = true)]
    async fn busy_remote_chat_uses_one_frozen_submission_and_terminal_budget() {
        let fake = FakeEngine::always_busy();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        let result = port
            .run_chat_turn_with_attachments_budget(
                "session-0001",
                "task-0001",
                TASK_MODEL_SOURCE_CONTROLLED,
                &lease(),
                "hello".into(),
                Vec::new(),
                Vec::new(),
                events(),
                CancellationToken::new(),
                TurnBudget::for_test(Duration::from_secs(1800)),
            )
            .await;
        assert_eq!(result.unwrap_err(), TURN_TIMEOUT_CODE);
        assert_eq!(fake.sent.lock().unwrap().as_slice(), ["hello"]);
        assert_eq!(fake.cancelled.lock().unwrap().as_slice(), ["session-0001"]);
    }

    #[tokio::test(start_paused = true)]
    async fn precancelled_remote_chat_never_reserves_or_submits() {
        let fake = FakeEngine::always_busy();
        let port = EnginePoolChatTurnPort::new(fake.clone());
        let cancel = CancellationToken::new();
        cancel.cancel();
        let result = port
            .run_chat_turn_with_attachments_budget(
                "session-0001",
                "task-0001",
                TASK_MODEL_SOURCE_CONTROLLED,
                &lease(),
                "hello".into(),
                Vec::new(),
                Vec::new(),
                events(),
                cancel,
                TurnBudget::for_test(Duration::from_secs(1)),
            )
            .await;
        assert_eq!(result.unwrap_err(), TASK_CANCELLED_CODE);
        assert!(fake.sent.lock().unwrap().is_empty());
        assert!(fake.cancelled.lock().unwrap().is_empty());
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

    #[test]
    fn assistant_delta_stream_redacts_tokens_split_across_callbacks() {
        let mut state = TurnEventRelayState {
            generation: Some(7),
            ..TurnEventRelayState::default()
        };
        assert_eq!(state.push_assistant("before sk-"), Some("before ".into()));
        assert_eq!(
            state.push_assistant("secret-value after "),
            Some("[REDACTED] after ".into())
        );
        assert_eq!(state.finish_assistant_token(true), None);
    }

    #[test]
    fn pending_projection_waits_while_other_safe_channel_batches_are_delivered() {
        let mut state = TurnEventRelayState {
            generation: Some(7),
            ..TurnEventRelayState::default()
        };
        assert_eq!(state.push_assistant("目录"), None);
        assert!(state.push_reasoning(Some(7), "推理 ").is_some());
        assert_eq!(take_reasoning_batch(&mut state, true), Some("推理 ".into()));
        assert_eq!(
            state.push_assistant("/秘密/file.txt "),
            Some("[PATH] ".into())
        );
        assert!(state.push_reasoning(Some(7), "推理目录").is_some());
        assert_eq!(take_reasoning_batch(&mut state, true), None);
        assert_eq!(state.push_assistant("正文 "), Some("正文 ".into()));
        assert!(state.push_reasoning(Some(7), "/秘密/file.txt ").is_some());
        assert_eq!(
            take_reasoning_batch(&mut state, true),
            Some("[PATH] ".into())
        );
        state.terminal = Some(Ok(()));
        assert_eq!(state.close_streams(true), (Vec::new(), None));
    }

    #[test]
    fn remote_projection_success_abort_and_duplicate_close_are_bounded() {
        for completed in [false, true] {
            for terminal in [
                None,
                Some(Ok(())),
                Some(Err(TASK_CANCELLED_CODE.to_owned())),
                Some(Err(TURN_TIMEOUT_CODE.to_owned())),
            ] {
                let mut state = TurnEventRelayState {
                    generation: Some(7),
                    ..TurnEventRelayState::default()
                };
                assert_eq!(state.push_assistant("普通正文"), None);
                assert!(state.push_reasoning(Some(7), "普通推理").is_some());
                state.terminal = terminal;
                let successful = completed && matches!(state.terminal, Some(Ok(())));
                let (reasoning, assistant) = state.close_streams(completed);
                assert_eq!(
                    reasoning,
                    vec![
                        if successful {
                            "普通推理"
                        } else {
                            "[REDACTED]"
                        }
                        .to_owned()
                    ]
                );
                assert_eq!(
                    assistant,
                    Some(
                        if successful {
                            "普通正文"
                        } else {
                            "[REDACTED]"
                        }
                        .to_owned()
                    )
                );
                assert_eq!(state.close_streams(true), (Vec::new(), None));
                assert_eq!(state.push_assistant("late "), None);
                assert_eq!(state.push_reasoning(Some(7), "late "), None);
            }
        }
        let mut state = TurnEventRelayState {
            generation: Some(7),
            terminal: Some(Ok(())),
            ..TurnEventRelayState::default()
        };
        assert_eq!(state.push_assistant("late "), None);
        let mut state = TurnEventRelayState {
            generation: Some(7),
            ..TurnEventRelayState::default()
        };
        assert_eq!(state.push_assistant("pending"), None);
        assert!(state.push_reasoning(Some(7), "pending").is_some());
        state.assistant_events = MAX_ASSISTANT_EVENTS_PER_TURN;
        state.emitted_events = MAX_REASONING_EVENTS_PER_TURN;
        assert_eq!(state.close_streams(false), (Vec::new(), None));
        let mut unarmed = TurnEventRelayState::default();
        assert_eq!(unarmed.push_reasoning(None, "must not leak "), None);
    }

    #[test]
    fn chat_delivery_cannot_be_overtaken_by_close_and_late_tools_are_rejected() {
        use parking_lot::Mutex;

        struct BarrierSink {
            prepared: std::sync::mpsc::Sender<()>,
            release: Mutex<std::sync::mpsc::Receiver<()>>,
            delivered: Mutex<Vec<(JournalKind, Value)>>,
        }
        impl ControllerTaskEventSink for BarrierSink {
            fn emit(&self, _: Value) {}
            fn emit_journal(&self, kind: JournalKind, _: &str, _: Option<&str>, payload: Value) {
                self.prepared.send(()).unwrap();
                self.release
                    .lock()
                    .recv_timeout(Duration::from_secs(5))
                    .unwrap();
                self.delivered.lock().push((kind, payload));
            }
        }
        let (prepared_tx, prepared_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::sync_channel(1);
        let sink = Arc::new(BarrierSink {
            prepared: prepared_tx,
            release: Mutex::new(release_rx),
            delivered: Mutex::new(Vec::new()),
        });
        let events: Arc<dyn ControllerTaskEventSink> = sink.clone();
        let state = Arc::new(Mutex::new(TurnEventRelayState {
            generation: Some(7),
            ..TurnEventRelayState::default()
        }));
        let worker_state = Arc::clone(&state);
        let worker_events = Arc::clone(&events);
        let worker = std::thread::spawn(move || {
            relay_chat_assistant(
                &worker_state,
                &worker_events,
                "session-1",
                "task-1",
                "safe ",
            );
        });
        prepared_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        // Output has been prepared but the sink has not delivered it. Close
        // must acquire this same lock, so it cannot cross this barrier.
        assert!(state.try_lock().is_none());
        assert!(sink.delivered.lock().is_empty());
        release_tx.send(()).unwrap();
        worker.join().unwrap();
        assert_eq!(state.lock().close_streams(false), (Vec::new(), None));
        for tool_state in ["running", "completed"] {
            relay_chat_tool(
                &state,
                &events,
                "session-1",
                "task-1",
                json!({"tool_state":tool_state}),
            );
        }
        relay_chat_assistant(&state, &events, "session-1", "task-1", "late ");
        assert_eq!(sink.delivered.lock().len(), 1);
        assert_eq!(
            sink.delivered.lock()[0],
            (JournalKind::AssistantDelta, json!({"text":"safe "}))
        );

        // A terminal is itself a fence, even before the handle closes.
        let mut guard = state.lock();
        guard.stopped = false;
        guard.terminal = Some(Ok(()));
        drop(guard);
        relay_chat_tool(&state, &events, "session-1", "task-1", json!({}));
        assert_eq!(sink.delivered.lock().len(), 1);
    }

    #[test]
    fn assistant_delta_stream_is_inactive_until_generation_is_armed() {
        let mut state = TurnEventRelayState::default();
        assert_eq!(state.push_assistant("must not leak"), None);
        state.generation = Some(1);
        assert_eq!(state.push_assistant("accepted "), Some("accepted ".into()));
    }
}
