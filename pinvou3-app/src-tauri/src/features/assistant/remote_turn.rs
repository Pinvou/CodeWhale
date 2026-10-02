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
    JournalKind, MAX_EVENT_TEXT_CHARS, StreamingTextSanitizer, sanitize_text,
};
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::task::{
    ControllerTaskEventSink, PublishedAttachment, RemoteChatTurnPort, TASK_CANCELLED_CODE,
    TASK_MODEL_SOURCE_CONTROLLED, TASK_MODEL_SOURCE_TEMPORARY,
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
const MAX_ASSISTANT_EVENTS_PER_TURN: usize = 256;
const MAX_ASSISTANT_CHARS_PER_TURN: usize = 128 * 1_024;

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
        skill_ids: Vec<String>,
        relay: Option<&TurnEventRelayHandle>,
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
        )
        .await
    }

    fn is_turn_active(&self, session_id: &str) -> bool;

    async fn cancel_turn(&self, session_id: &str);

    async fn release_task_model(&self, session_id: &str, model_source: &str);

    async fn restore_session_skills(&self, session_id: &str) -> std::result::Result<(), String>;

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
    assistant_sanitizer: StreamingTextSanitizer,
    assistant_events: usize,
    assistant_chars: usize,
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

    fn push_assistant(&mut self, text: &str) -> Option<String> {
        if self.stopped
            || self.generation.is_none()
            || self.assistant_events >= MAX_ASSISTANT_EVENTS_PER_TURN
            || self.assistant_chars >= MAX_ASSISTANT_CHARS_PER_TURN
        {
            return None;
        }
        let sanitized = self.assistant_sanitizer.push(text);
        self.bound_assistant(sanitized)
    }

    fn finish_assistant_token(&mut self) -> Option<String> {
        let tail = self.assistant_sanitizer.finish();
        self.bound_assistant(tail)
    }

    fn bound_assistant(&mut self, text: String) -> Option<String> {
        if text.is_empty() {
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

    fn finish(&mut self) {
        for listener_id in self.listener_ids.drain(..) {
            self.app.unlisten(listener_id);
        }
        let (reasoning_batches, assistant_tail) = {
            let mut state = self.state.lock();
            if state.stopped {
                return;
            }
            state.stopped = true;
            let tail = state.reasoning_sanitizer.finish();
            state.reasoning_ready.push_str(&tail);
            let mut reasoning_batches = Vec::new();
            while let Some(batch) = take_reasoning_batch(&mut state, true) {
                reasoning_batches.push(batch);
            }
            let assistant_tail = state.finish_assistant_token();
            (reasoning_batches, assistant_tail)
        };
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
        self.finish();
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
    async fn send_user_message(
        &self,
        session_id: &str,
        task_id: &str,
        model_source: &str,
        lease: &LeaseContext,
        content: String,
        skill_ids: Vec<String>,
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
            .send_remote_user_message(
                session_id,
                content,
                skill_ids,
                model_override,
                |generation| {
                    if let Some(relay) = relay {
                        relay.arm(generation);
                    }
                },
            )
            .await
            .map_err(|_| "task_engine_unavailable".to_owned())
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
                )
                .await;
        }
        let root = self
            .pool
            .session_state_root(session_id)
            .map_err(|_| "task_session_not_found".to_owned())?;
        // Retain the process-only publication owners through the complete
        // engine turn, including later image/tool path reads and cancellation.
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
                |generation| {
                    if let Some(relay) = relay {
                        relay.arm(generation);
                    }
                },
            )
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

    async fn restore_session_skills(&self, session_id: &str) -> std::result::Result<(), String> {
        let session_id = session_id.to_owned();
        tokio::task::spawn_blocking(move || {
            super::skill_materialization::restore_plain_session_skills(&session_id)
        })
        .await
        .map_err(|_| "task_skill_restore_failed".to_owned())?
        .map_err(|_| "task_skill_restore_failed".to_owned())
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
            // A reasoning token that preceded this assistant delta must reach
            // the authoritative journal first, even when its batching timer
            // has not fired yet.
            flush_reasoning(
                &assistant_state,
                &assistant_events,
                &assistant_session_id,
                &assistant_task_id,
                true,
            );
            let text = assistant_state.lock().push_assistant(text);
            if let Some(text) = text {
                assistant_events.emit_journal(
                    JournalKind::AssistantDelta,
                    &assistant_session_id,
                    Some(&assistant_task_id),
                    json!({ "text": text }),
                );
            }
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
                || tool_start_state.lock().generation.is_none()
            {
                return;
            }
            flush_reasoning(
                &tool_start_state,
                &tool_start_events,
                &tool_start_session_id,
                &tool_start_task_id,
                true,
            );
            if let Some(text) = tool_start_state.lock().finish_assistant_token() {
                tool_start_events.emit_journal(
                    JournalKind::AssistantDelta,
                    &tool_start_session_id,
                    Some(&tool_start_task_id),
                    json!({ "text": text }),
                );
            }
            let Some(tool_id) = payload.get("id").and_then(Value::as_str) else {
                return;
            };
            let Some(tool_name) = payload.get("name").and_then(Value::as_str) else {
                return;
            };
            // Never project `args`: tool arguments can contain credentials,
            // signed URLs, or B-local host paths.
            tool_start_events.emit_journal(
                JournalKind::Tool,
                &tool_start_session_id,
                Some(&tool_start_task_id),
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
                || tool_end_state.lock().generation.is_none()
            {
                return;
            }
            flush_reasoning(
                &tool_end_state,
                &tool_end_events,
                &tool_end_session_id,
                &tool_end_task_id,
                true,
            );
            if let Some(text) = tool_end_state.lock().finish_assistant_token() {
                tool_end_events.emit_journal(
                    JournalKind::AssistantDelta,
                    &tool_end_session_id,
                    Some(&tool_end_task_id),
                    json!({ "text": text }),
                );
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
            tool_end_events.emit_journal(
                JournalKind::Tool,
                &tool_end_session_id,
                Some(&tool_end_task_id),
                json!({
                    "tool_id": tool_id,
                    "tool_name": tool_name,
                    "tool_state": if success { "completed" } else { "failed" },
                    "text": output,
                }),
            );
        });

        Some(TurnEventRelayHandle {
            app: self.app.clone(),
            listener_ids: vec![
                reasoning_listener,
                assistant_listener,
                tool_start_listener,
                tool_end_listener,
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
        let mut turn_relay = self.engine.start_turn_relay(session_id, task_id, events);
        let result = async {
            tokio::select! {
                biased;
                _ = cancel.cancelled() => {
                    self.engine.cancel_turn(session_id).await;
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
                ) => result?,
            }
            self.wait_for_terminal(session_id, &cancel).await?;
            Ok(())
        }
        .await;
        // Flush the last redaction-safe token before the task terminal event.
        // Assistant content is journaled from real `chat:delta` callbacks,
        // never synthesized as one final response here.
        if let Some(relay) = turn_relay.as_mut() {
            relay.finish();
        }
        // The frozen selection is turn-scoped. Restore B's authoritative
        // enabled set before the remote task releases its shared task lock, so
        // the next B-local turn cannot inherit A's previous selection.
        let restore_result = self.engine.restore_session_skills(session_id).await;
        // EnginePool entries retain their prepared runtime credential. A
        // task-scoped A credential must therefore evict the engine on every
        // success/failure/cancel path before the model store is released.
        self.engine
            .release_task_model(session_id, model_source)
            .await;
        match (result, restore_result) {
            (Ok(()), Ok(())) => Ok(None),
            (Ok(()), Err(error)) => Err(error),
            (Err(error), _) => Err(error),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

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
            _skill_ids: Vec<String>,
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

        async fn restore_session_skills(
            &self,
            _session_id: &str,
        ) -> std::result::Result<(), String> {
            Ok(())
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
                Vec::new(),
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
        assert_eq!(state.finish_assistant_token(), None);
    }

    #[test]
    fn assistant_delta_stream_is_inactive_until_generation_is_armed() {
        let mut state = TurnEventRelayState::default();
        assert_eq!(state.push_assistant("must not leak"), None);
        state.generation = Some(1);
        assert_eq!(state.push_assistant("accepted "), Some("accepted ".into()));
    }
}
