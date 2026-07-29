//! Durable thread/turn/item runtime for the HTTP API and background tasks.
//!
//! Execution follows the configured provider route while exposing Codex-like
//! lifecycle semantics (threads, turns, items, interrupt/steer, and replayable
//! events).

// Background-task runtime — runs alongside the TUI. Raw stdio prints
// here would still land in the alt-screen on whichever terminal the
// foreground TUI happens to own. Route everything through `tracing::*`
// instead — see `runtime_log` for the rationale.
#![deny(clippy::print_stdout)]
#![deny(clippy::print_stderr)]

use std::collections::{HashMap, HashSet, VecDeque};
use std::fs::{self, File, OpenOptions};
use std::io::{BufRead, BufReader, Write};
use std::path::{Component, Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result, anyhow, bail};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::sync::{Mutex, broadcast, oneshot};
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

use crate::compaction::CompactionConfig;
#[cfg(test)]
use crate::config::DEFAULT_TEXT_MODEL;
use crate::config::{ApiProvider, Config, MAX_SUBAGENTS, ProviderIdentity};
use crate::core::engine::{
    EngineConfig, EngineHandle, spawn_engine_with_authoritative_route_config,
};
use crate::core::events::{Event as EngineEvent, TurnOutcomeStatus};
use crate::core::ops::Op;
use crate::models::{ContentBlock, Message, SystemPrompt, Usage};
use crate::route_budget::{
    auto_compact_default_for_route, compaction_threshold_for_route_at_percent, known_route_limits,
    route_context_window_tokens,
};
use crate::route_runtime::{
    ResolvedRuntimeRoute, resolve_runtime_route, resolve_runtime_route_for_identity,
};
use crate::tools::plan::new_shared_plan_state;
use crate::tools::subagent::SubAgentStatus;
use crate::tools::todo::new_shared_todo_list;
use crate::tui::app::AppMode;
use codewhale_protocol::runtime::{
    DynamicToolCallContent, DynamicToolCallParams, DynamicToolCallResult, DynamicToolSpec,
    TurnEnvironmentParams,
};

const EVENT_CHANNEL_CAPACITY: usize = 1024;
const MAX_ACTIVE_THREADS_DEFAULT: usize = 8;
const SUMMARY_LIMIT: usize = 280;

/// Sentinel delimiters wrapping the compaction summary section persisted in a
/// thread record's `system_prompt`. The section carries the engine-rendered
/// summary (which contains the `Conversation Summary (Auto-Generated)` marker,
/// so `SyncSession` → `extract_compaction_summary_prompt` restores it on
/// engine reload). Delimiters make replacement idempotent: each completed
/// compaction swaps the section in place instead of stacking duplicates.
/// External `PATCH /v1/threads/{id}` callers that rewrite `system_prompt`
/// should preserve this section verbatim or the summary is lost on reload.
const COMPACTION_SUMMARY_BEGIN: &str = "<!-- compaction-summary:begin -->";
const COMPACTION_SUMMARY_END: &str = "<!-- compaction-summary:end -->";

/// Merge a rendered compaction summary into a thread record's system prompt,
/// replacing any previously persisted summary section.
fn merge_summary_into_prompt(base: Option<&str>, summary_text: &str) -> String {
    let stripped = base.map(strip_summary_section).unwrap_or_default();
    let mut out = stripped.trim_end().to_string();
    if !out.is_empty() {
        out.push_str("\n\n");
    }
    out.push_str(COMPACTION_SUMMARY_BEGIN);
    out.push('\n');
    out.push_str(summary_text.trim());
    out.push('\n');
    out.push_str(COMPACTION_SUMMARY_END);
    out
}

/// Remove a previously persisted compaction summary section, if present.
fn strip_summary_section(base: &str) -> String {
    let Some(start) = base.find(COMPACTION_SUMMARY_BEGIN) else {
        return base.to_string();
    };
    let end = base[start..]
        .find(COMPACTION_SUMMARY_END)
        .map(|rel| start + rel + COMPACTION_SUMMARY_END.len());
    let mut out = base[..start].trim_end().to_string();
    if let Some(end) = end {
        let tail = base[end..].trim_start();
        if !tail.is_empty() {
            if !out.is_empty() {
                out.push_str("\n\n");
            }
            out.push_str(tail);
        }
    }
    out
}

fn validated_record_id<'a>(id: &'a str, label: &str) -> Result<&'a str> {
    let trimmed = id.trim();
    if trimmed.is_empty() {
        bail!("{label} cannot be empty");
    }
    if trimmed != id {
        bail!("{label} cannot contain leading or trailing whitespace");
    }
    if !trimmed
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
    {
        bail!("{label} contains unsupported characters");
    }
    Ok(trimmed)
}

fn sort_turn_items_by_start(items: &mut [TurnItemRecord]) {
    let fallback = Utc::now();
    items.sort_by(|a, b| {
        let left = a.started_at.unwrap_or(fallback);
        let right = b.started_at.unwrap_or(fallback);
        left.cmp(&right)
    });
}

/// Bumped to 2 for v0.6.6 after live engine semantics changed. The persisted
/// thread/turn/item records did not change shape, but a v1 reader on a v2
/// session should still fail closed rather than silently mis-replay.
const CURRENT_RUNTIME_SCHEMA_VERSION: u32 = 2;
const RUNTIME_RESTART_REASON: &str = "Interrupted by process restart";
const EMPTY_TURN_REASON: &str = "Turn completed without engine output";
const APPROVAL_DECISION_TIMEOUT: Duration = Duration::from_secs(300);

#[cfg(test)]
static TEST_APPROVAL_DECISION_TIMEOUT_MS: std::sync::atomic::AtomicU64 =
    std::sync::atomic::AtomicU64::new(0);

fn approval_decision_timeout() -> Duration {
    #[cfg(test)]
    {
        let ms = TEST_APPROVAL_DECISION_TIMEOUT_MS.load(std::sync::atomic::Ordering::SeqCst);
        if ms > 0 {
            return Duration::from_millis(ms);
        }
    }
    APPROVAL_DECISION_TIMEOUT
}

#[cfg(test)]
pub(crate) fn set_test_approval_decision_timeout_ms(ms: u64) -> u64 {
    TEST_APPROVAL_DECISION_TIMEOUT_MS.swap(ms, std::sync::atomic::Ordering::SeqCst)
}

const fn default_runtime_schema_version() -> u32 {
    CURRENT_RUNTIME_SCHEMA_VERSION
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum RuntimeTurnStatus {
    Queued,
    InProgress,
    Completed,
    Failed,
    Interrupted,
    Canceled,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum TurnItemKind {
    UserMessage,
    AgentMessage,
    AgentReasoning,
    ToolCall,
    FileChange,
    CommandExecution,
    ContextCompaction,
    Status,
    Error,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum TurnItemLifecycleStatus {
    Queued,
    InProgress,
    Completed,
    Failed,
    Interrupted,
    Canceled,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ThreadRecord {
    #[serde(default = "default_runtime_schema_version")]
    pub schema_version: u32,
    pub id: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub model: String,
    /// Generic provider kind for this thread's model route. Named custom
    /// routes remain `custom` for compatibility with enum-only consumers.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model_provider: Option<String>,
    /// Exact non-secret configured provider key.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model_provider_id: Option<String>,
    pub workspace: PathBuf,
    pub mode: String,
    pub allow_shell: bool,
    pub trust_mode: bool,
    pub auto_approve: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub latest_turn_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub latest_response_bookmark: Option<String>,
    #[serde(default)]
    pub archived: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub system_prompt: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub task_id: Option<String>,
    /// User-set title for the thread. When `None`, consumers fall back to a
    /// derived title (typically the latest turn's input summary). Added in
    /// v0.8.10 (#562); old runtime records simply have no `title` and behave
    /// as before. Schema version is not bumped because this field is purely
    /// additive metadata — older readers ignore it without misinterpretation.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub title: Option<String>,
    /// The session ID associated with this thread. When set, `ensure_engine_loaded`
    /// loads the full message history (including thinking/tool blocks) from the
    /// session file instead of reconstructing from turns (which loses process info).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub session_id: Option<String>,
}

fn thread_execution_state_matches(left: &ThreadRecord, right: &ThreadRecord) -> bool {
    left.schema_version == right.schema_version
        && left.id == right.id
        && left.model == right.model
        && left.model_provider == right.model_provider
        && left.model_provider_id == right.model_provider_id
        && left.workspace == right.workspace
        && left.mode == right.mode
        && left.allow_shell == right.allow_shell
        && left.trust_mode == right.trust_mode
        && left.auto_approve == right.auto_approve
        && left.latest_turn_id == right.latest_turn_id
        && left.latest_response_bookmark == right.latest_response_bookmark
        && left.archived == right.archived
        && left.system_prompt == right.system_prompt
        && left.task_id == right.task_id
        && left.session_id == right.session_id
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TurnRecord {
    #[serde(default = "default_runtime_schema_version")]
    pub schema_version: u32,
    pub id: String,
    pub thread_id: String,
    pub status: RuntimeTurnStatus,
    pub input_summary: String,
    pub created_at: DateTime<Utc>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub started_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ended_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub duration_ms: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub usage: Option<Usage>,
    /// Concrete generic provider kind selected for this turn.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub effective_provider: Option<String>,
    /// Exact non-secret configured provider key selected for this turn.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub effective_provider_id: Option<String>,
    /// Non-secret discriminator for routes whose provider/model pair spans
    /// different billing systems (for example StepFun PAYG vs Step Plan).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub effective_billing_surface: Option<String>,
    /// Concrete wire model selected for this turn (especially important when
    /// the thread is configured as `auto`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub effective_model: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    #[serde(default)]
    pub item_ids: Vec<String>,
    #[serde(default)]
    pub steer_count: usize,
}

impl TurnRecord {
    pub(crate) fn effective_provider_label(&self) -> Option<&str> {
        self.effective_provider_id
            .as_deref()
            .filter(|identity| !identity.trim().is_empty())
            .or_else(|| {
                self.effective_provider
                    .as_deref()
                    .filter(|provider| !provider.trim().is_empty())
            })
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TurnItemRecord {
    #[serde(default = "default_runtime_schema_version")]
    pub schema_version: u32,
    pub id: String,
    pub turn_id: String,
    pub kind: TurnItemKind,
    pub status: TurnItemLifecycleStatus,
    pub summary: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub metadata: Option<Value>,
    #[serde(default)]
    pub artifact_refs: Vec<PathBuf>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub started_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ended_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RuntimeEventRecord {
    #[serde(default = "default_runtime_schema_version")]
    pub schema_version: u32,
    pub seq: u64,
    pub timestamp: DateTime<Utc>,
    pub thread_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub turn_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub item_id: Option<String>,
    pub event: String,
    pub payload: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RuntimeStoreState {
    #[serde(default = "default_runtime_schema_version")]
    schema_version: u32,
    next_seq: u64,
}

impl Default for RuntimeStoreState {
    fn default() -> Self {
        Self {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            next_seq: 1,
        }
    }
}

#[derive(Debug, Clone)]
pub struct RuntimeThreadStore {
    threads_dir: PathBuf,
    turns_dir: PathBuf,
    items_dir: PathBuf,
    events_dir: PathBuf,
    state_path: PathBuf,
    state: Arc<Mutex<RuntimeStoreState>>,
    /// Serializes load-modify-save operations on thread records. The guard is
    /// synchronous and must never cross an `.await`; JSON records are small,
    /// and one global guard avoids per-thread lock lifecycle races.
    thread_mutation: Arc<parking_lot::Mutex<()>>,
    /// Serializes load-modify-save operations on turn records. Like the
    /// thread guard, it is synchronous and never crosses an `.await`.
    turn_mutation: Arc<parking_lot::Mutex<()>>,
}

impl RuntimeThreadStore {
    pub fn open(root: PathBuf) -> Result<Self> {
        let root = checked_runtime_store_root(root)?;
        let threads_dir = root.join("threads");
        let turns_dir = root.join("turns");
        let items_dir = root.join("items");
        let events_dir = root.join("events");
        ensure_runtime_store_dir(&threads_dir)?;
        ensure_runtime_store_dir(&turns_dir)?;
        ensure_runtime_store_dir(&items_dir)?;
        ensure_runtime_store_dir(&events_dir)?;

        let state_path = root.join("state.json");
        reject_symlinked_store_file(&state_path)?;
        let state = if state_path.exists() {
            let raw = read_store_file(&state_path)?;
            serde_json::from_str::<RuntimeStoreState>(&raw)
                .with_context(|| format!("Failed to parse {}", state_path.display()))?
        } else {
            let default = RuntimeStoreState::default();
            write_json_atomic(&state_path, &default)?;
            default
        };

        Ok(Self {
            threads_dir,
            turns_dir,
            items_dir,
            events_dir,
            state_path,
            state: Arc::new(Mutex::new(state)),
            thread_mutation: Arc::new(parking_lot::Mutex::new(())),
            turn_mutation: Arc::new(parking_lot::Mutex::new(())),
        })
    }

    fn record_path(base: &Path, id: &str, extension: &str, label: &str) -> Result<PathBuf> {
        let id = validated_record_id(id, label)?;
        Ok(base.join(format!("{id}.{extension}")))
    }

    fn thread_path(&self, thread_id: &str) -> Result<PathBuf> {
        Self::record_path(&self.threads_dir, thread_id, "json", "thread id")
    }

    fn turn_path(&self, turn_id: &str) -> Result<PathBuf> {
        Self::record_path(&self.turns_dir, turn_id, "json", "turn id")
    }

    fn item_path(&self, item_id: &str) -> Result<PathBuf> {
        Self::record_path(&self.items_dir, item_id, "json", "item id")
    }

    fn events_path(&self, thread_id: &str) -> Result<PathBuf> {
        Self::record_path(&self.events_dir, thread_id, "jsonl", "thread id")
    }

    pub fn save_thread(&self, thread: &ThreadRecord) -> Result<()> {
        write_json_atomic(&self.thread_path(&thread.id)?, thread)
    }

    pub fn save_turn(&self, turn: &TurnRecord) -> Result<()> {
        validated_record_id(&turn.thread_id, "thread id")?;
        write_json_atomic(&self.turn_path(&turn.id)?, turn)
    }

    pub fn save_item(&self, item: &TurnItemRecord) -> Result<()> {
        validated_record_id(&item.turn_id, "turn id")?;
        write_json_atomic(&self.item_path(&item.id)?, item)
    }

    fn remove_turn(&self, turn_id: &str) -> Result<()> {
        remove_file_if_exists(&self.turn_path(turn_id)?)
    }

    fn remove_thread(&self, thread_id: &str) -> Result<()> {
        remove_file_if_exists(&self.thread_path(thread_id)?)
    }

    fn remove_item(&self, item_id: &str) -> Result<()> {
        remove_file_if_exists(&self.item_path(item_id)?)
    }

    pub fn load_thread(&self, thread_id: &str) -> Result<ThreadRecord> {
        let path = self.thread_path(thread_id)?;
        let raw = read_store_file(&path)
            .with_context(|| format!("Failed to read thread {}", path.display()))?;
        let record: ThreadRecord = serde_json::from_str(&raw)
            .with_context(|| format!("Failed to parse thread {}", path.display()))?;
        if record.schema_version > CURRENT_RUNTIME_SCHEMA_VERSION {
            bail!(
                "Thread schema v{} is newer than supported v{}",
                record.schema_version,
                CURRENT_RUNTIME_SCHEMA_VERSION
            );
        }
        Ok(record)
    }

    pub fn load_turn(&self, turn_id: &str) -> Result<TurnRecord> {
        let path = self.turn_path(turn_id)?;
        let raw = read_store_file(&path)
            .with_context(|| format!("Failed to read turn {}", path.display()))?;
        let record: TurnRecord = serde_json::from_str(&raw)
            .with_context(|| format!("Failed to parse turn {}", path.display()))?;
        if record.schema_version > CURRENT_RUNTIME_SCHEMA_VERSION {
            bail!(
                "Turn schema v{} is newer than supported v{}",
                record.schema_version,
                CURRENT_RUNTIME_SCHEMA_VERSION
            );
        }
        Ok(record)
    }

    pub fn load_item(&self, item_id: &str) -> Result<TurnItemRecord> {
        let path = self.item_path(item_id)?;
        let raw = read_store_file(&path)
            .with_context(|| format!("Failed to read item {}", path.display()))?;
        let record: TurnItemRecord = serde_json::from_str(&raw)
            .with_context(|| format!("Failed to parse item {}", path.display()))?;
        if record.schema_version > CURRENT_RUNTIME_SCHEMA_VERSION {
            bail!(
                "Item schema v{} is newer than supported v{}",
                record.schema_version,
                CURRENT_RUNTIME_SCHEMA_VERSION
            );
        }
        Ok(record)
    }

    pub fn list_threads(&self) -> Result<Vec<ThreadRecord>> {
        let mut out = Vec::new();
        let threads_dir = checked_existing_runtime_store_dir(&self.threads_dir)?;
        for entry in fs::read_dir(&threads_dir)
            .with_context(|| format!("Failed to read {}", threads_dir.display()))?
        {
            let entry = entry?;
            let path = entry.path();
            if path.extension().is_none_or(|ext| ext != "json") {
                continue;
            }
            let raw = read_store_file(&path)
                .with_context(|| format!("Failed to read {}", path.display()))?;
            let thread: ThreadRecord = serde_json::from_str(&raw)
                .with_context(|| format!("Failed to parse {}", path.display()))?;
            if thread.schema_version > CURRENT_RUNTIME_SCHEMA_VERSION {
                bail!(
                    "Thread schema v{} is newer than supported v{}",
                    thread.schema_version,
                    CURRENT_RUNTIME_SCHEMA_VERSION
                );
            }
            out.push(thread);
        }
        out.sort_by_key(|t| std::cmp::Reverse(t.updated_at));
        Ok(out)
    }

    pub fn list_turns_for_thread(&self, thread_id: &str) -> Result<Vec<TurnRecord>> {
        validated_record_id(thread_id, "thread id")?;
        let mut out = self.list_all_turns()?;
        out.retain(|turn| turn.thread_id == thread_id);
        Ok(out)
    }

    /// Every turn in the store, sorted by creation time. One directory scan;
    /// callers that need multiple threads' turns (boot recovery) use this
    /// instead of paying a full scan per thread (#3757).
    pub fn list_all_turns(&self) -> Result<Vec<TurnRecord>> {
        let mut out = Vec::new();
        let turns_dir = checked_existing_runtime_store_dir(&self.turns_dir)?;
        for entry in fs::read_dir(&turns_dir)
            .with_context(|| format!("Failed to read {}", turns_dir.display()))?
        {
            let entry = entry?;
            let path = entry.path();
            if path.extension().is_none_or(|ext| ext != "json") {
                continue;
            }
            let raw = read_store_file(&path)
                .with_context(|| format!("Failed to read {}", path.display()))?;
            let turn: TurnRecord = serde_json::from_str(&raw)
                .with_context(|| format!("Failed to parse {}", path.display()))?;
            if turn.schema_version > CURRENT_RUNTIME_SCHEMA_VERSION {
                bail!(
                    "Turn schema v{} is newer than supported v{}",
                    turn.schema_version,
                    CURRENT_RUNTIME_SCHEMA_VERSION
                );
            }
            out.push(turn);
        }
        out.sort_by_key(|a| a.created_at);
        Ok(out)
    }

    pub fn list_items_for_turn(&self, turn_id: &str) -> Result<Vec<TurnItemRecord>> {
        validated_record_id(turn_id, "turn id")?;
        let mut out = Vec::new();
        let items_dir = checked_existing_runtime_store_dir(&self.items_dir)?;
        for entry in fs::read_dir(&items_dir)
            .with_context(|| format!("Failed to read {}", items_dir.display()))?
        {
            let entry = entry?;
            let path = entry.path();
            if path.extension().is_none_or(|ext| ext != "json") {
                continue;
            }
            let raw = read_store_file(&path)
                .with_context(|| format!("Failed to read {}", path.display()))?;
            let item: TurnItemRecord = serde_json::from_str(&raw)
                .with_context(|| format!("Failed to parse {}", path.display()))?;
            if item.schema_version > CURRENT_RUNTIME_SCHEMA_VERSION {
                bail!(
                    "Item schema v{} is newer than supported v{}",
                    item.schema_version,
                    CURRENT_RUNTIME_SCHEMA_VERSION
                );
            }
            if item.turn_id == turn_id {
                out.push(item);
            }
        }
        sort_turn_items_by_start(&mut out);
        Ok(out)
    }

    pub fn list_items_for_turns_map(
        &self,
        turn_ids: &[String],
    ) -> Result<HashMap<String, Vec<TurnItemRecord>>> {
        if turn_ids.is_empty() {
            return Ok(HashMap::new());
        }

        for turn_id in turn_ids {
            validated_record_id(turn_id, "turn id")?;
        }

        let wanted: HashSet<&str> = turn_ids.iter().map(String::as_str).collect();
        let mut out: HashMap<String, Vec<TurnItemRecord>> = HashMap::new();
        let items_dir = checked_existing_runtime_store_dir(&self.items_dir)?;
        for entry in fs::read_dir(&items_dir)
            .with_context(|| format!("Failed to read {}", items_dir.display()))?
        {
            let entry = entry?;
            let path = entry.path();
            if path.extension().is_none_or(|ext| ext != "json") {
                continue;
            }
            let raw = read_store_file(&path)
                .with_context(|| format!("Failed to read {}", path.display()))?;
            let item: TurnItemRecord = serde_json::from_str(&raw)
                .with_context(|| format!("Failed to parse {}", path.display()))?;
            if item.schema_version > CURRENT_RUNTIME_SCHEMA_VERSION {
                bail!(
                    "Item schema v{} is newer than supported v{}",
                    item.schema_version,
                    CURRENT_RUNTIME_SCHEMA_VERSION
                );
            }
            if wanted.contains(item.turn_id.as_str()) {
                out.entry(item.turn_id.clone()).or_default().push(item);
            }
        }

        for items in out.values_mut() {
            sort_turn_items_by_start(items);
        }
        Ok(out)
    }

    pub async fn append_event(
        &self,
        thread_id: &str,
        turn_id: Option<&str>,
        item_id: Option<&str>,
        event: impl Into<String>,
        payload: Value,
    ) -> Result<RuntimeEventRecord> {
        validated_record_id(thread_id, "thread id")?;
        if let Some(turn_id) = turn_id {
            validated_record_id(turn_id, "turn id")?;
        }
        if let Some(item_id) = item_id {
            validated_record_id(item_id, "item id")?;
        }
        let path = self.events_path(thread_id)?;
        reject_symlinked_store_dir(&self.events_dir)?;
        reject_symlinked_store_file(&path)?;

        let mut state = self.state.lock().await;
        let seq = state.next_seq;
        state.next_seq = state.next_seq.saturating_add(1);
        write_json_atomic(&self.state_path, &*state)?;

        let record = RuntimeEventRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            seq,
            timestamp: Utc::now(),
            thread_id: thread_id.to_string(),
            turn_id: turn_id.map(ToString::to_string),
            item_id: item_id.map(ToString::to_string),
            event: event.into(),
            payload,
        };

        let mut file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&path)
            .with_context(|| format!("Failed to open {}", path.display()))?;
        let line = serde_json::to_string(&record)?;
        writeln!(file, "{line}").with_context(|| format!("Failed to append {}", path.display()))?;
        file.flush()
            .with_context(|| format!("Failed to flush {}", path.display()))?;
        file.sync_all()
            .with_context(|| format!("Failed to fsync {}", path.display()))?;
        // Keep the global sequence lock through the append so no later event
        // can reach disk or broadcast before this sequence number.
        drop(state);
        Ok(record)
    }

    pub fn events_since(
        &self,
        thread_id: &str,
        since_seq: Option<u64>,
    ) -> Result<Vec<RuntimeEventRecord>> {
        let path = self.events_path(thread_id)?;
        reject_symlinked_store_dir(&self.events_dir)?;
        reject_symlinked_store_file(&path)?;
        if !path.exists() {
            return Ok(Vec::new());
        }
        let file =
            File::open(&path).with_context(|| format!("Failed to open {}", path.display()))?;
        let reader = BufReader::new(file);
        let mut out = Vec::new();
        for line in reader.lines() {
            let line = line?;
            if line.trim().is_empty() {
                continue;
            }
            let event: RuntimeEventRecord = serde_json::from_str(&line)
                .with_context(|| format!("Failed to parse event line in {}", path.display()))?;
            if let Some(since) = since_seq
                && event.seq <= since
            {
                continue;
            }
            out.push(event);
        }
        Ok(out)
    }

    pub async fn current_seq(&self) -> u64 {
        let state = self.state.lock().await;
        state.next_seq.saturating_sub(1)
    }
}

#[derive(Debug, Clone)]
pub struct RuntimeThreadManagerConfig {
    pub data_dir: PathBuf,
    pub task_data_dir: PathBuf,
    pub max_active_threads: usize,
}

impl RuntimeThreadManagerConfig {
    #[must_use]
    pub fn from_task_data_dir(task_data_dir: PathBuf) -> Self {
        let data_dir = if let Ok(override_dir) = std::env::var("DEEPSEEK_RUNTIME_DIR") {
            if override_dir.trim().is_empty() {
                task_data_dir.join("runtime")
            } else {
                PathBuf::from(override_dir)
            }
        } else {
            task_data_dir.join("runtime")
        };
        Self {
            data_dir,
            task_data_dir,
            max_active_threads: MAX_ACTIVE_THREADS_DEFAULT,
        }
    }
}

/// Visibility filter for `list_threads`. Default is `ActiveOnly`. The runtime
/// API exposes this as the combination of `include_archived` and
/// `archived_only` query params (see `runtime_api.rs`); whalescale#260 / #563.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum ThreadListFilter {
    /// Only `archived = false` threads. The original default.
    #[default]
    ActiveOnly,
    /// Active and archived threads, sorted as the store returns them.
    IncludeArchived,
    /// Only `archived = true` threads.
    ArchivedOnly,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct CreateThreadRequest {
    pub model: Option<String>,
    /// Generic provider kind or, for legacy clients, an exact provider id.
    #[serde(default)]
    pub model_provider: Option<String>,
    /// Exact configured provider key. Takes precedence over `model_provider`.
    #[serde(default)]
    pub model_provider_id: Option<String>,
    pub workspace: Option<PathBuf>,
    pub mode: Option<String>,
    pub allow_shell: Option<bool>,
    pub trust_mode: Option<bool>,
    pub auto_approve: Option<bool>,
    #[serde(default)]
    pub archived: bool,
    #[serde(default)]
    pub system_prompt: Option<String>,
    #[serde(default)]
    pub task_id: Option<String>,
    #[serde(default)]
    pub dynamic_tools: Vec<DynamicToolSpec>,
    #[serde(default)]
    pub environments: Vec<TurnEnvironmentParams>,
}

/// Mutable fields accepted by `PATCH /v1/threads/{id}`.
///
/// Each field is optional — missing means "no change". Extended in v0.8.10
/// (#562, whalescale#256) so the UI can flip persistent thread state without
/// having to recreate a thread or pass per-turn overrides on every send.
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct UpdateThreadRequest {
    pub archived: Option<bool>,
    pub allow_shell: Option<bool>,
    pub trust_mode: Option<bool>,
    pub auto_approve: Option<bool>,
    pub model: Option<String>,
    pub mode: Option<String>,
    pub title: Option<String>,
    pub system_prompt: Option<String>,
    pub workspace: Option<PathBuf>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StartTurnRequest {
    pub prompt: String,
    #[serde(default)]
    pub input_summary: Option<String>,
    pub model: Option<String>,
    pub mode: Option<String>,
    pub allow_shell: Option<bool>,
    pub trust_mode: Option<bool>,
    pub auto_approve: Option<bool>,
    #[serde(default)]
    pub dynamic_tools: Vec<DynamicToolSpec>,
    #[serde(default)]
    pub environment_id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SteerTurnRequest {
    pub prompt: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct CompactThreadRequest {
    #[serde(default)]
    pub reason: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ThreadDetail {
    pub thread: ThreadRecord,
    pub turns: Vec<TurnRecord>,
    pub items: Vec<TurnItemRecord>,
    pub latest_seq: u64,
}

/// Aggregation key for `aggregate_usage`. Whalescale#261 / #564.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UsageGroupBy {
    Day,
    Model,
    Provider,
    Thread,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct UsageTotals {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cached_tokens: u64,
    pub reasoning_tokens: u64,
    pub cost_usd: f64,
    pub turns: u64,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct UsageBucket {
    pub key: String,
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cached_tokens: u64,
    pub reasoning_tokens: u64,
    pub cost_usd: f64,
    pub turns: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct UsageAggregation {
    pub since: Option<DateTime<Utc>>,
    pub until: Option<DateTime<Utc>>,
    pub group_by: String,
    pub totals: UsageTotals,
    pub buckets: Vec<UsageBucket>,
}

fn resolve_runtime_thread_route(
    config: &Config,
    provider: ApiProvider,
    model_selector: Option<&str>,
) -> Result<ResolvedRuntimeRoute> {
    resolve_runtime_route(config, provider, model_selector)
        .map_err(|reason| anyhow!("Failed to resolve runtime thread route: {reason}"))
}

fn resolve_runtime_thread_route_for_identity(
    config: &Config,
    identity: &ProviderIdentity,
    model_selector: Option<&str>,
) -> Result<ResolvedRuntimeRoute> {
    resolve_runtime_route_for_identity(config, identity, model_selector)
        .map_err(|reason| anyhow!("Failed to resolve runtime thread route: {reason}"))
}

fn runtime_compaction_config(
    provider: ApiProvider,
    model: &str,
    route_limits: Option<codewhale_config::route::RouteLimits>,
    auto_compact: bool,
    auto_compact_explicit: bool,
    threshold_percent: f64,
) -> CompactionConfig {
    CompactionConfig {
        enabled: if auto_compact_explicit {
            auto_compact
        } else {
            auto_compact_default_for_route(provider, model, route_limits)
        },
        model: model.to_string(),
        token_threshold: compaction_threshold_for_route_at_percent(
            provider,
            model,
            route_limits,
            threshold_percent,
        ),
        effective_context_window: Some(route_context_window_tokens(provider, model, route_limits)),
        ..Default::default()
    }
}

#[derive(Debug, Clone)]
struct ActiveTurnState {
    turn_id: String,
    interrupt_requested: bool,
    auto_approve: bool,
    trust_mode: bool,
}

#[derive(Debug, Clone, Copy)]
enum ClaimedTurnKind {
    Message,
    Compaction,
}

impl ClaimedTurnKind {
    const fn label(self) -> &'static str {
        match self {
            Self::Message => "turn",
            Self::Compaction => "compaction turn",
        }
    }
}

#[derive(Clone)]
struct ActiveThreadState {
    engine: EngineHandle,
    active_turn: Option<ActiveTurnState>,
    route_identity: ProviderIdentity,
    route_model: String,
    /// Real engines client-preflight before an in-progress record is written.
    /// Explicitly injected test engines own their client seam.
    client_preflight_required: bool,
}

#[derive(Default)]
struct ActiveThreads {
    engines: HashMap<String, ActiveThreadState>,
    lru: VecDeque<String>,
}

pub type SharedRuntimeThreadManager = Arc<RuntimeThreadManager>;

/// Manages active engine threads, lifecycle, and event persistence.
///
/// # Lock ordering invariant
///
/// Runtime state uses six locks:
/// - `RuntimeThreadManager::engine_load` — serializes cache-miss engine builds.
///   It may cross awaits and is always acquired before `active`.
/// - `RuntimeThreadManager::event_emit` — preserves append-to-broadcast event
///   order and is only acquired after all record/engine guards are released.
/// - `RuntimeThreadStore::state` — protects the monotonic event sequence counter.
/// - `RuntimeThreadStore::thread_mutation` — synchronizes short, synchronous
///   thread-record load-modify-save transactions and never crosses `.await`.
/// - `RuntimeThreadStore::turn_mutation` — does the same for turn records.
/// - `RuntimeThreadManager::active` — protects the set of loaded engine handles.
///
/// `state` is never held with `active`, either record-mutation guard, or
/// `engine_load`. Event publication intentionally acquires `event_emit` before
/// `state`; both guards are released before `emit_event` returns. All
/// `emit_event` calls happen after `active`, `thread_mutation`, and
/// `turn_mutation` have been released. When record and engine state must change
/// atomically, acquire `active` before the applicable record-mutation guard and
/// release both before awaiting.
#[derive(Clone)]
pub struct RuntimeThreadManager {
    config: Arc<parking_lot::RwLock<Config>>,
    workspace: PathBuf,
    store: RuntimeThreadStore,
    engine_load: Arc<Mutex<()>>,
    active: Arc<Mutex<ActiveThreads>>,
    event_emit: Arc<Mutex<()>>,
    event_tx: broadcast::Sender<RuntimeEventRecord>,
    manager_cfg: RuntimeThreadManagerConfig,
    cancel_token: CancellationToken,
    task_manager: Arc<parking_lot::Mutex<Option<crate::task_manager::SharedTaskManager>>>,
    automations:
        Arc<parking_lot::Mutex<Option<crate::automation_manager::SharedAutomationManager>>>,
    pending_approvals:
        Arc<parking_lot::Mutex<HashMap<String, oneshot::Sender<ExternalApprovalDecision>>>>,
    pending_dynamic_tools:
        Arc<parking_lot::Mutex<HashMap<String, oneshot::Sender<DynamicToolCallResult>>>>,
}

/// Helper types for `seed_thread_from_messages` — intermediate representation
/// of a turn being built from session messages before persisting as items.
///
/// A single content block extracted from an assistant message.
enum SeedItem {
    Text(String),
    Thinking(String),
    ToolUse {
        id: String,
        name: String,
        input: serde_json::Value,
    },
    ToolResult {
        tool_use_id: String,
        content: String,
        is_error: bool,
        content_blocks: Option<Vec<serde_json::Value>>,
    },
}

/// A turn being assembled from session messages.
struct TurnSeed {
    user_text: String,
    items: Vec<SeedItem>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RuntimeApprovalDecision {
    ApproveTool,
    DenyTool,
    RetryWithFullAccess,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ExternalApprovalDecision {
    Allow { remember: bool },
    Deny { remember: bool },
}

impl RuntimeThreadManager {
    /// Helper to read the current config under RwLock.
    pub(crate) fn read_config(&self) -> parking_lot::RwLockReadGuard<'_, Config> {
        self.config.read()
    }

    fn resolved_route_for_thread(
        &self,
        config: &Config,
        thread: &ThreadRecord,
    ) -> Result<ResolvedRuntimeRoute> {
        let provider_identity = self.provider_identity_for_thread(config, thread)?;
        if !thread.model.trim().eq_ignore_ascii_case("auto") {
            return resolve_runtime_thread_route_for_identity(
                config,
                &provider_identity,
                Some(&thread.model),
            );
        }

        let mut thread_config = config.clone();
        thread_config.scope_to_provider_identity(&provider_identity);

        let restored = self
            .store
            .list_turns_for_thread(&thread.id)?
            .into_iter()
            .rev()
            .find_map(|turn| {
                let model = turn.effective_model?.trim().to_string();
                let provider_kind = turn
                    .effective_provider
                    .filter(|provider| !provider.trim().is_empty());
                // Preserve an explicitly empty additive id so malformed
                // imported receipts fail closed instead of becoming an
                // id-less legacy custom route.
                let provider_id = turn.effective_provider_id;
                ((provider_kind.is_some() || provider_id.is_some()) && !model.is_empty())
                    .then_some((provider_kind, provider_id, model))
            });
        match restored {
            Some((restored_kind, restored_id, model)) => {
                let identity = thread_config
                    .resolve_persisted_provider_identity(
                        restored_kind.as_deref(),
                        restored_id.as_deref(),
                    )
                    .map_err(|reason| anyhow!(reason))?;
                resolve_runtime_thread_route_for_identity(config, &identity, Some(&model))
            }
            None => resolve_runtime_thread_route_for_identity(config, &provider_identity, None),
        }
    }

    fn provider_identity_for_thread(
        &self,
        config: &Config,
        thread: &ThreadRecord,
    ) -> Result<ProviderIdentity> {
        let has_persisted_route = thread
            .model_provider
            .as_deref()
            .is_some_and(|provider| !provider.trim().is_empty())
            || thread.model_provider_id.is_some();
        let identity = if has_persisted_route {
            config.resolve_persisted_provider_identity(
                thread.model_provider.as_deref(),
                thread.model_provider_id.as_deref(),
            )
        } else {
            config.active_provider_identity(config.api_provider())
        };
        identity.map_err(|reason| anyhow!(reason))
    }

    /// Atomically replace the authoritative runtime config after preflighting
    /// every loaded thread's exact route. Active turns retain their immutable
    /// descriptor; the next `start_turn` resolves and installs the new route.
    pub async fn reload_config(&self, new_config: Config) -> Result<()> {
        let _engine_load = self.engine_load.lock().await;
        let entries: Vec<(String, EngineHandle, ProviderIdentity, String)> = {
            let active = self.active.lock().await;
            active
                .engines
                .iter()
                .map(|(id, state)| {
                    (
                        id.clone(),
                        state.engine.clone(),
                        state.route_identity.clone(),
                        state.route_model.clone(),
                    )
                })
                .collect()
        };

        let mut validated = Vec::with_capacity(entries.len());
        let mut failures = Vec::new();
        for (thread_id, engine, provider_identity, engine_model) in entries {
            match resolve_runtime_thread_route_for_identity(
                &new_config,
                &provider_identity,
                Some(&engine_model),
            ) {
                Ok(route) => validated.push((thread_id, engine, route)),
                Err(err) => failures.push(format!("{thread_id}: {err}")),
            }
        }
        if !failures.is_empty() {
            bail!(
                "Config reload rejected because active thread routes are invalid: {}",
                failures.join("; ")
            );
        }

        {
            let mut guard = self.config.write();
            *guard = new_config;
        }

        let settings = crate::settings::Settings::load().unwrap_or_default();
        let stream_chunk_timeout_secs = self.read_config().stream_chunk_timeout_secs();
        for (thread_id, engine, route) in validated {
            let provider = route.identity.provider;
            let route_limits = known_route_limits(route.candidate.limits);
            let engine_compaction = runtime_compaction_config(
                provider,
                &route.model,
                route_limits,
                settings.auto_compact,
                crate::settings::Settings::auto_compact_explicitly_configured(),
                settings.auto_compact_threshold_percent,
            );
            let route_config = route.config;
            let _ = engine
                .send(Op::SetCompaction {
                    config: engine_compaction,
                })
                .await;
            let _ = engine
                .send(Op::SetStreamChunkTimeout {
                    timeout_secs: stream_chunk_timeout_secs,
                })
                .await;
            let _ = engine
                .send(Op::SetSubagentRuntimeConfig {
                    enabled: route_config.subagents_enabled_for_provider(provider),
                    max_subagents: route_config
                        .max_subagents_for_provider(provider)
                        .clamp(1, crate::config::MAX_SUBAGENTS),
                    launch_concurrency: route_config.launch_concurrency_for_provider(provider),
                    max_spawn_depth: route_config.subagent_max_spawn_depth_for_provider(provider),
                    api_timeout_secs: route_config.subagent_api_timeout_secs_for_provider(provider),
                    heartbeat_timeout_secs: route_config
                        .subagent_heartbeat_timeout_secs_for_provider(provider),
                })
                .await;
            tracing::info!(
                thread_id = %thread_id,
                "Reloaded runtime controls; provider route will apply on the next turn"
            );
        }
        Ok(())
    }

    pub fn open(
        config: Config,
        workspace: PathBuf,
        manager_cfg: RuntimeThreadManagerConfig,
    ) -> Result<Self> {
        let store = RuntimeThreadStore::open(manager_cfg.data_dir.clone())?;
        let (event_tx, _event_rx) = broadcast::channel(EVENT_CHANNEL_CAPACITY);
        let manager = Self {
            config: Arc::new(parking_lot::RwLock::new(config)),
            workspace,
            store,
            engine_load: Arc::new(Mutex::new(())),
            active: Arc::new(Mutex::new(ActiveThreads::default())),
            event_emit: Arc::new(Mutex::new(())),
            event_tx,
            manager_cfg,
            cancel_token: CancellationToken::new(),
            task_manager: Arc::new(parking_lot::Mutex::new(None)),
            automations: Arc::new(parking_lot::Mutex::new(None)),
            pending_approvals: Arc::new(parking_lot::Mutex::new(HashMap::new())),
            pending_dynamic_tools: Arc::new(parking_lot::Mutex::new(HashMap::new())),
        };
        manager.recover_interrupted_state()?;
        Ok(manager)
    }

    /// Attach the durable task manager so model-visible task tools work inside
    /// runtime thread turns as well as interactive TUI turns.
    pub fn attach_task_manager(&self, task_manager: crate::task_manager::SharedTaskManager) {
        *self.task_manager.lock() = Some(task_manager);
    }

    /// Attach the automation manager for model-visible scheduling tools.
    pub fn attach_automation_manager(
        &self,
        automations: crate::automation_manager::SharedAutomationManager,
    ) {
        *self.automations.lock() = Some(automations);
    }

    #[allow(dead_code)] // Public API for external callers (runtime API, task manager)
    pub fn shutdown(&self) {
        self.cancel_token.cancel();
        self.pending_approvals.lock().clear();
        self.pending_dynamic_tools.lock().clear();
    }

    #[allow(dead_code)] // Public API for external callers
    pub fn is_shutdown(&self) -> bool {
        self.cancel_token.is_cancelled()
    }

    fn register_pending_approval(
        &self,
        approval_id: &str,
    ) -> oneshot::Receiver<ExternalApprovalDecision> {
        let (tx, rx) = oneshot::channel();
        self.pending_approvals
            .lock()
            .insert(approval_id.to_string(), tx);
        rx
    }

    fn cancel_pending_approval(&self, approval_id: &str) {
        self.pending_approvals.lock().remove(approval_id);
    }

    fn register_pending_dynamic_tool(
        &self,
        call_id: &str,
    ) -> oneshot::Receiver<DynamicToolCallResult> {
        let (tx, rx) = oneshot::channel();
        self.pending_dynamic_tools
            .lock()
            .insert(call_id.to_string(), tx);
        rx
    }

    fn cancel_pending_dynamic_tool(&self, call_id: &str) {
        self.pending_dynamic_tools.lock().remove(call_id);
    }

    pub fn deliver_external_approval(
        &self,
        approval_id: &str,
        decision: ExternalApprovalDecision,
    ) -> bool {
        let sender = self.pending_approvals.lock().remove(approval_id);
        match sender {
            Some(tx) => tx.send(decision).is_ok(),
            None => false,
        }
    }

    pub fn deliver_dynamic_tool_result(
        &self,
        call_id: &str,
        result: DynamicToolCallResult,
    ) -> bool {
        let sender = self.pending_dynamic_tools.lock().remove(call_id);
        match sender {
            Some(tx) => tx.send(result).is_ok(),
            None => false,
        }
    }

    pub async fn submit_user_input(
        &self,
        thread_id: &str,
        input_id: &str,
        response: crate::tools::user_input::UserInputResponse,
    ) -> Result<bool> {
        let active = self.active.lock().await;
        let Some(state) = active.engines.get(thread_id) else {
            bail!("thread '{thread_id}' not found");
        };
        state.engine.submit_user_input(input_id, response).await?;
        Ok(true)
    }

    #[allow(dead_code)]
    pub async fn cancel_user_input(&self, thread_id: &str, input_id: &str) -> Result<bool> {
        let active = self.active.lock().await;
        let Some(state) = active.engines.get(thread_id) else {
            bail!("thread '{thread_id}' not found");
        };
        state.engine.cancel_user_input(input_id).await?;
        Ok(true)
    }

    #[allow(dead_code)]
    pub fn pending_approvals_count(&self) -> usize {
        self.pending_approvals.lock().len()
    }

    #[allow(dead_code)]
    pub fn pending_dynamic_tools_count(&self) -> usize {
        self.pending_dynamic_tools.lock().len()
    }

    #[cfg(test)]
    pub(crate) fn register_pending_approval_for_test(
        &self,
        approval_id: &str,
    ) -> oneshot::Receiver<ExternalApprovalDecision> {
        self.register_pending_approval(approval_id)
    }

    #[cfg(test)]
    pub(crate) fn register_pending_dynamic_tool_for_test(
        &self,
        call_id: &str,
    ) -> oneshot::Receiver<DynamicToolCallResult> {
        self.register_pending_dynamic_tool(call_id)
    }

    async fn remember_thread_auto_approve(&self, thread_id: &str) {
        {
            let _thread_mutation = self.store.thread_mutation.lock();
            let Ok(mut thread) = self.store.load_thread(thread_id) else {
                return;
            };
            if thread.auto_approve {
                return;
            }
            thread.auto_approve = true;
            thread.updated_at = Utc::now();
            if let Err(err) = self.store.save_thread(&thread) {
                tracing::warn!(
                    "Failed to persist auto_approve flip for thread {}: {}",
                    thread_id,
                    err
                );
            }
        }

        {
            let mut active = self.active.lock().await;
            if let Some(state) = active.engines.get_mut(thread_id)
                && let Some(turn) = state.active_turn.as_mut()
            {
                turn.auto_approve = true;
            }
        }
    }

    #[must_use]
    pub fn subscribe_events(&self) -> broadcast::Receiver<RuntimeEventRecord> {
        self.event_tx.subscribe()
    }

    async fn emit_event(
        &self,
        thread_id: &str,
        turn_id: Option<&str>,
        item_id: Option<&str>,
        event: impl Into<String>,
        payload: Value,
    ) -> Result<RuntimeEventRecord> {
        let _emit_order = self.event_emit.lock().await;
        let record = self
            .store
            .append_event(thread_id, turn_id, item_id, event, payload)
            .await?;
        if let Err(e) = self.event_tx.send(record.clone()) {
            tracing::debug!(
                "Runtime event broadcast failed (no receivers or channel full): {}",
                e
            );
        }
        Ok(record)
    }

    pub async fn create_thread(&self, req: CreateThreadRequest) -> Result<ThreadRecord> {
        let now = Utc::now();
        let (model_provider, model_provider_id, default_model) = {
            let config = self.read_config().clone();
            let requested_kind = req
                .model_provider
                .as_deref()
                .filter(|provider| !provider.trim().is_empty());
            // `Some("")` is malformed provenance, not absence. Pass it to
            // the resolver so an imported/API-created record cannot silently
            // acquire the root custom route.
            let requested_id = req.model_provider_id.as_deref().map(str::trim);
            let identity = if requested_kind.is_some() || requested_id.is_some() {
                config.resolve_persisted_provider_identity(requested_kind, requested_id)
            } else {
                let selected = config
                    .provider
                    .as_deref()
                    .unwrap_or(ApiProvider::Deepseek.as_str());
                config.resolve_provider_identity(selected)
            }
            .map_err(|reason| anyhow!(reason))?;
            let default_model = resolve_runtime_route_for_identity(&config, &identity, None)
                .map_err(|reason| anyhow!(reason))?
                .model;
            (
                identity.provider.as_str().to_string(),
                identity.exact_id,
                default_model,
            )
        };
        let model = req
            .model
            .filter(|m| !m.trim().is_empty())
            .unwrap_or(default_model);
        let workspace = req.workspace.unwrap_or_else(|| self.workspace.clone());
        let mode = req
            .mode
            .filter(|m| !m.trim().is_empty())
            .unwrap_or_else(|| "agent".to_string());
        let allow_shell = req
            .allow_shell
            .unwrap_or_else(|| self.read_config().allow_shell());
        let trust_mode = req.trust_mode.unwrap_or(false);
        let auto_approve = req.auto_approve.unwrap_or(false);

        let thread = ThreadRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            id: format!("thr_{}", &Uuid::new_v4().to_string()[..8]),
            created_at: now,
            updated_at: now,
            model,
            model_provider: Some(model_provider),
            model_provider_id,
            workspace,
            mode,
            allow_shell,
            trust_mode,
            auto_approve,
            latest_turn_id: None,
            latest_response_bookmark: None,
            archived: req.archived,
            system_prompt: req.system_prompt,
            task_id: req.task_id,
            title: None,
            session_id: None,
        };
        self.store.save_thread(&thread)?;
        self.emit_event(
            &thread.id,
            None,
            None,
            "thread.started",
            json!({ "thread": thread }),
        )
        .await?;
        Ok(thread)
    }

    pub async fn list_threads(
        &self,
        filter: ThreadListFilter,
        limit: Option<usize>,
    ) -> Result<Vec<ThreadRecord>> {
        let mut threads = self.store.list_threads()?;
        match filter {
            ThreadListFilter::ActiveOnly => threads.retain(|t| !t.archived),
            ThreadListFilter::ArchivedOnly => threads.retain(|t| t.archived),
            ThreadListFilter::IncludeArchived => {}
        }
        if let Some(limit) = limit {
            threads.truncate(limit);
        }
        Ok(threads)
    }

    /// Aggregate token + cost usage across all threads/turns inside the time
    /// range `[since, until]`. Each turn's cost is computed via
    /// provider-aware pricing using each turn's persisted concrete route.
    /// Legacy turns without provider provenance and providers without an
    /// authoritative runtime price (including ChatGPT/Codex OAuth) accrue
    /// tokens but no fabricated dollar cost. Whalescale#261 / #564.
    ///
    /// Buckets are sorted by ascending key for deterministic output. Empty
    /// ranges produce empty `buckets` (never an error).
    pub async fn aggregate_usage(
        &self,
        since: Option<DateTime<Utc>>,
        until: Option<DateTime<Utc>>,
        group_by: UsageGroupBy,
    ) -> Result<UsageAggregation> {
        use std::collections::BTreeMap;

        let mut buckets: BTreeMap<String, UsageBucket> = BTreeMap::new();
        let mut totals = UsageTotals::default();
        for thread in self.store.list_threads()? {
            let turns = self.store.list_turns_for_thread(&thread.id)?;
            for turn in turns {
                if let Some(s) = since
                    && turn.created_at < s
                {
                    continue;
                }
                if let Some(u) = until
                    && turn.created_at > u
                {
                    continue;
                }
                let Some(usage) = turn.usage.as_ref() else {
                    continue;
                };
                let cached = usage.prompt_cache_hit_tokens.unwrap_or(0) as u64;
                let reasoning = usage.reasoning_tokens.unwrap_or(0) as u64;
                let input = usage.input_tokens as u64;
                let output = usage.output_tokens as u64;
                let model = turn
                    .effective_model
                    .as_deref()
                    .filter(|model| !model.trim().is_empty())
                    .unwrap_or(&thread.model);
                let provider_kind = turn
                    .effective_provider
                    .as_deref()
                    .filter(|provider| !provider.trim().is_empty())
                    .unwrap_or("unknown");
                let provider_label = turn.effective_provider_label().unwrap_or(provider_kind);
                let provider = ApiProvider::parse(provider_kind);
                let cost = provider
                    .and_then(|provider| {
                        crate::pricing::calculate_turn_cost_estimate_for_route_at(
                            provider,
                            model,
                            turn.effective_billing_surface.as_deref(),
                            usage,
                            turn.created_at,
                        )
                    })
                    .map(|estimate| estimate.usd)
                    .unwrap_or(0.0);

                totals.input_tokens += input;
                totals.output_tokens += output;
                totals.cached_tokens += cached;
                totals.reasoning_tokens += reasoning;
                totals.cost_usd += cost;
                totals.turns += 1;

                let key = match group_by {
                    UsageGroupBy::Day => turn.created_at.format("%Y-%m-%d").to_string(),
                    UsageGroupBy::Model => model.to_string(),
                    UsageGroupBy::Provider => provider_label.to_string(),
                    UsageGroupBy::Thread => thread.id.clone(),
                };
                let bucket = buckets.entry(key.clone()).or_insert_with(|| UsageBucket {
                    key,
                    ..UsageBucket::default()
                });
                bucket.input_tokens += input;
                bucket.output_tokens += output;
                bucket.cached_tokens += cached;
                bucket.reasoning_tokens += reasoning;
                bucket.cost_usd += cost;
                bucket.turns += 1;
            }
        }

        let group_by_str = match group_by {
            UsageGroupBy::Day => "day",
            UsageGroupBy::Model => "model",
            UsageGroupBy::Provider => "provider",
            UsageGroupBy::Thread => "thread",
        }
        .to_string();

        Ok(UsageAggregation {
            since,
            until,
            group_by: group_by_str,
            totals,
            buckets: buckets.into_values().collect(),
        })
    }

    pub async fn get_thread(&self, id: &str) -> Result<ThreadRecord> {
        self.store
            .load_thread(id)
            .with_context(|| format!("Thread not found: {id}"))
    }

    pub async fn update_thread(&self, id: &str, req: UpdateThreadRequest) -> Result<ThreadRecord> {
        if req.archived.is_none()
            && req.allow_shell.is_none()
            && req.trust_mode.is_none()
            && req.auto_approve.is_none()
            && req.model.is_none()
            && req.mode.is_none()
            && req.title.is_none()
            && req.system_prompt.is_none()
            && req.workspace.is_none()
        {
            bail!("At least one thread field is required");
        }

        if let Some(model) = req.model.as_ref()
            && model.trim().is_empty()
        {
            bail!("model must not be empty");
        }
        if let Some(mode) = req.mode.as_ref()
            && mode.trim().is_empty()
        {
            bail!("mode must not be empty");
        }
        if let Some(workspace) = req.workspace.as_ref()
            && workspace.as_os_str().is_empty()
        {
            bail!("workspace must not be empty");
        }

        let (thread, changes, evicted_engine) = {
            // Take the active guard first so a workspace mutation can check
            // and evict the cached engine atomically with the durable update.
            // Using the same order as start/compact avoids lock inversion.
            let mut active = self.active.lock().await;
            let _thread_mutation = self.store.thread_mutation.lock();
            let mut thread = self
                .store
                .load_thread(id)
                .with_context(|| format!("Thread not found: {id}"))?;
            let mut changes = serde_json::Map::new();

            if let Some(archived) = req.archived
                && thread.archived != archived
            {
                thread.archived = archived;
                changes.insert("archived".to_string(), json!(archived));
            }
            if let Some(allow_shell) = req.allow_shell
                && thread.allow_shell != allow_shell
            {
                thread.allow_shell = allow_shell;
                changes.insert("allow_shell".to_string(), json!(allow_shell));
            }
            if let Some(trust_mode) = req.trust_mode
                && thread.trust_mode != trust_mode
            {
                thread.trust_mode = trust_mode;
                changes.insert("trust_mode".to_string(), json!(trust_mode));
            }
            if let Some(auto_approve) = req.auto_approve
                && thread.auto_approve != auto_approve
            {
                thread.auto_approve = auto_approve;
                changes.insert("auto_approve".to_string(), json!(auto_approve));
            }
            if let Some(model) = req.model
                && thread.model != model
            {
                thread.model = model.clone();
                changes.insert("model".to_string(), json!(model));
            }
            if let Some(mode) = req.mode
                && thread.mode != mode
            {
                thread.mode = mode.clone();
                changes.insert("mode".to_string(), json!(mode));
            }
            if let Some(title) = req.title {
                // Empty string clears a previously-set title and reverts to derived.
                let new_title = if title.trim().is_empty() {
                    None
                } else {
                    Some(title)
                };
                if thread.title != new_title {
                    thread.title = new_title.clone();
                    changes.insert("title".to_string(), json!(new_title));
                }
            }
            if let Some(system_prompt) = req.system_prompt {
                let new_sys = if system_prompt.trim().is_empty() {
                    None
                } else {
                    Some(system_prompt)
                };
                if thread.system_prompt != new_sys {
                    thread.system_prompt = new_sys.clone();
                    changes.insert("system_prompt".to_string(), json!(new_sys));
                }
            }
            if let Some(workspace) = req.workspace
                && thread.workspace != workspace
            {
                changes.insert("workspace".to_string(), json!(workspace));
                thread.workspace = workspace;
            }

            let workspace_changed = changes.contains_key("workspace");
            if workspace_changed
                && active
                    .engines
                    .get(id)
                    .and_then(|state| state.active_turn.as_ref())
                    .is_some()
            {
                bail!("workspace cannot be changed while the thread has an active turn");
            }

            let evicted_engine = if changes.is_empty() {
                None
            } else {
                thread.updated_at = Utc::now();
                self.store.save_thread(&thread)?;
                if workspace_changed {
                    active.lru.retain(|thread_id| thread_id != id);
                    active.engines.remove(id).map(|state| state.engine)
                } else {
                    None
                }
            };
            (thread, changes, evicted_engine)
        };

        if let Some(engine) = evicted_engine {
            let _ = engine.send(Op::Shutdown).await;
        }

        if !changes.is_empty() {
            self.emit_event(
                &thread.id,
                None,
                None,
                "thread.updated",
                json!({
                    "thread": thread.clone(),
                    "changes": Value::Object(changes),
                }),
            )
            .await?;
        }

        Ok(thread)
    }

    /// Link a session to a thread so that `ensure_engine_loaded` can restore
    /// the full message history (including thinking/tool blocks) from the
    /// session file instead of reconstructing from turns.
    pub async fn set_thread_session_id(&self, thread_id: &str, session_id: &str) -> Result<()> {
        let thread = {
            let _thread_mutation = self.store.thread_mutation.lock();
            let mut thread = self
                .store
                .load_thread(thread_id)
                .with_context(|| format!("Thread not found: {thread_id}"))?;
            if thread.session_id.as_deref() == Some(session_id) {
                return Ok(());
            }
            thread.session_id = Some(session_id.to_string());
            thread.updated_at = Utc::now();
            self.store.save_thread(&thread)?;
            thread
        };
        self.emit_event(
            thread_id,
            None,
            None,
            "thread.updated",
            json!({ "thread": thread, "changes": { "session_id": session_id } }),
        )
        .await?;
        Ok(())
    }

    pub async fn get_thread_detail(&self, id: &str) -> Result<ThreadDetail> {
        let thread = self.get_thread(id).await?;
        let turns = self.store.list_turns_for_thread(id)?;
        let turn_ids: Vec<String> = turns.iter().map(|turn| turn.id.clone()).collect();
        let mut items_by_turn = self.store.list_items_for_turns_map(&turn_ids)?;
        let mut items = Vec::new();
        for turn in &turns {
            if let Some(mut turn_items) = items_by_turn.remove(&turn.id) {
                items.append(&mut turn_items);
            }
        }
        let latest_seq = self.store.current_seq().await;
        Ok(ThreadDetail {
            thread,
            turns,
            items,
            latest_seq,
        })
    }

    pub async fn resume_thread(&self, id: &str) -> Result<ThreadRecord> {
        let thread = self.get_thread(id).await?;
        self.ensure_engine_loaded(&thread).await?;
        Ok(thread)
    }

    /// Resume a thread and recover the sub-agent rebind hints needed to
    /// reconstruct in-transcript cards (issue #128). Drains the persisted
    /// `agent.*` event stream and collapses it into the latest known
    /// status per `agent_id` — the UI consumes this to seed empty
    /// `DelegateCard` / `FanoutCard` placeholders so subsequent live
    /// mailbox envelopes mutate them in place.
    #[allow(dead_code)] // exposed for the runtime API resume flow; consumed by #128 follow-up.
    pub async fn resume_thread_with_agent_rebind(
        &self,
        id: &str,
    ) -> Result<(ThreadRecord, Vec<AgentRebindHint>)> {
        let thread = self.resume_thread(id).await?;
        let events = self.store.events_since(&thread.id, None)?;
        let hints = collect_agent_rebind_hints(&events);
        Ok((thread, hints))
    }

    pub async fn fork_thread(&self, id: &str) -> Result<ThreadRecord> {
        let source = self.get_thread(id).await?;
        let mut forked = source.clone();
        let now = Utc::now();
        forked.id = format!("thr_{}", &Uuid::new_v4().to_string()[..8]);
        forked.created_at = now;
        forked.updated_at = now;
        forked.latest_turn_id = None;
        forked.archived = false;

        let source_turns = self.store.list_turns_for_thread(&source.id)?;
        let mut cloned_records = Vec::with_capacity(source_turns.len());
        for source_turn in source_turns {
            let mut cloned_turn = source_turn.clone();
            cloned_turn.id = format!("turn_{}", &Uuid::new_v4().to_string()[..8]);
            cloned_turn.thread_id = forked.id.clone();
            cloned_turn.item_ids.clear();

            let items = self.store.list_items_for_turn(&source_turn.id)?;
            let mut cloned_items = Vec::with_capacity(items.len());
            for item in items {
                let mut cloned_item = item.clone();
                cloned_item.id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
                cloned_item.turn_id = cloned_turn.id.clone();
                cloned_turn.item_ids.push(cloned_item.id.clone());
                cloned_items.push(cloned_item);
            }
            forked.latest_turn_id = Some(cloned_turn.id.clone());
            forked.updated_at = now;
            cloned_records.push((cloned_turn, cloned_items));
        }
        self.publish_fork(&forked, &cloned_records)?;

        self.emit_event(
            &forked.id,
            None,
            None,
            "thread.forked",
            json!({
                "thread": forked,
                "source_thread_id": source.id,
            }),
        )
        .await?;
        Ok(forked)
    }

    /// Fork a thread, dropping every turn from the Nth-from-tail user
    /// message onward (issue #133 — Esc-Esc backtrack).
    ///
    /// `depth_from_tail` selects which user turn to roll back *to*:
    ///
    /// - `0` — drop the most recent turn (the freshest user message and
    ///   everything after it)
    /// - `1` — drop the two most recent turns (rewind one further)
    /// - …and so on
    ///
    /// Returns a tuple of `(forked_thread, original_user_text)` where the
    /// second element is the `detail` of the first `UserMessage` item in
    /// the *first dropped* turn — i.e. the input the user typed to start
    /// that turn — so the caller can pre-populate the composer with it.
    /// `None` when no detail was recorded (defensive — every persisted
    /// `UserMessage` since v0.6 carries a detail string).
    ///
    /// Counts user turns by iterating `list_turns_for_thread` (sorted
    /// oldest → newest) backwards. A turn is counted as a "user turn"
    /// when at least one of its items has `kind ==
    /// TurnItemKind::UserMessage`. Steered turns (which append additional
    /// `UserMessage` items) still count as one turn — backtrack rewinds
    /// at the turn boundary, not at the steer boundary.
    ///
    /// Errors:
    /// - `depth_from_tail` exceeds the number of user turns
    /// - source thread not found
    #[allow(dead_code)] // exposed for the runtime/HTTP fork-on-backtrack path; the in-TUI Esc-Esc flow trims `App` state directly. Issue #133.
    pub async fn fork_at_user_message(
        &self,
        id: &str,
        depth_from_tail: usize,
    ) -> Result<(ThreadRecord, Option<String>)> {
        let source = self.get_thread(id).await?;
        let source_turns = self.store.list_turns_for_thread(&source.id)?;

        // Walk turns from newest to oldest. For each turn, ask: does it
        // contain a UserMessage item? If yes, it counts toward the depth.
        let mut user_turn_indices: Vec<usize> = Vec::new();
        for (idx, turn) in source_turns.iter().enumerate().rev() {
            let items = self.store.list_items_for_turn(&turn.id)?;
            if items
                .iter()
                .any(|item| item.kind == TurnItemKind::UserMessage)
            {
                user_turn_indices.push(idx);
            }
        }
        if depth_from_tail >= user_turn_indices.len() {
            bail!(
                "fork_at_user_message: depth {} exceeds {} user turn(s)",
                depth_from_tail,
                user_turn_indices.len()
            );
        }
        // `user_turn_indices` is newest-first because we iterated in
        // reverse, so the Nth element is exactly the Nth-from-tail user
        // turn in the original chronological list.
        let target_turn_idx = user_turn_indices[depth_from_tail];
        let target_turn_id = source_turns[target_turn_idx].id.clone();

        // Pull the original user-message text out of the dropped turn so
        // the caller can drop it back into the composer.
        let target_items = self.store.list_items_for_turn(&target_turn_id)?;
        let original_user_text = target_items
            .iter()
            .find(|item| item.kind == TurnItemKind::UserMessage)
            .and_then(|item| item.detail.clone());

        // Copy turns strictly before `target_turn_idx` into a new thread.
        // Mirrors `fork_thread` but stops at the cutoff instead of copying
        // every turn. Kept structurally close so future parity reviews
        // can spot drift between the two paths.
        let mut forked = source.clone();
        let now = Utc::now();
        forked.id = format!("thr_{}", &Uuid::new_v4().to_string()[..8]);
        forked.created_at = now;
        forked.updated_at = now;
        forked.latest_turn_id = None;
        forked.archived = false;

        let mut cloned_records = Vec::with_capacity(target_turn_idx);
        for source_turn in source_turns.iter().take(target_turn_idx) {
            let mut cloned_turn = source_turn.clone();
            cloned_turn.id = format!("turn_{}", &Uuid::new_v4().to_string()[..8]);
            cloned_turn.thread_id = forked.id.clone();
            cloned_turn.item_ids.clear();

            let items = self.store.list_items_for_turn(&source_turn.id)?;
            let mut cloned_items = Vec::with_capacity(items.len());
            for item in items {
                let mut cloned_item = item.clone();
                cloned_item.id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
                cloned_item.turn_id = cloned_turn.id.clone();
                cloned_turn.item_ids.push(cloned_item.id.clone());
                cloned_items.push(cloned_item);
            }
            forked.latest_turn_id = Some(cloned_turn.id.clone());
            forked.updated_at = now;
            cloned_records.push((cloned_turn, cloned_items));
        }
        self.publish_fork(&forked, &cloned_records)?;

        self.emit_event(
            &forked.id,
            None,
            None,
            "thread.forked",
            json!({
                "thread": forked,
                "source_thread_id": source.id,
                "backtrack_depth_from_tail": depth_from_tail,
                "dropped_turn_id": target_turn_id,
            }),
        )
        .await?;
        Ok((forked, original_user_text))
    }

    /// Persist cloned records before publishing their thread. Until the final
    /// atomic thread write succeeds, list/get/start callers cannot observe a
    /// partial fork. Any failed write removes all unpublished clone artifacts.
    fn publish_fork(
        &self,
        thread: &ThreadRecord,
        records: &[(TurnRecord, Vec<TurnItemRecord>)],
    ) -> Result<()> {
        let mut saved_turn_ids = Vec::new();
        let mut saved_item_ids = Vec::new();
        let persistence = (|| -> Result<()> {
            for (turn, items) in records {
                for item in items {
                    self.store.save_item(item)?;
                    saved_item_ids.push(item.id.clone());
                }
                self.store.save_turn(turn)?;
                saved_turn_ids.push(turn.id.clone());
            }
            self.store.save_thread(thread)
        })();

        if let Err(persistence_error) = persistence {
            let mut cleanup_errors = Vec::new();
            if let Err(error) = self.store.remove_thread(&thread.id) {
                cleanup_errors.push(format!("remove thread: {error}"));
            }
            for turn_id in saved_turn_ids.iter().rev() {
                if let Err(error) = self.store.remove_turn(turn_id) {
                    cleanup_errors.push(format!("remove turn {turn_id}: {error}"));
                }
            }
            for item_id in saved_item_ids.iter().rev() {
                if let Err(error) = self.store.remove_item(item_id) {
                    cleanup_errors.push(format!("remove item {item_id}: {error}"));
                }
            }
            if cleanup_errors.is_empty() {
                return Err(persistence_error);
            }
            bail!(
                "Failed to persist fork: {persistence_error}; cleanup also failed: {}",
                cleanup_errors.join("; ")
            );
        }
        Ok(())
    }

    /// Seed a thread with messages from a saved session so subsequent turns
    /// continue with the prior conversation context.
    ///
    /// Unlike the old text-only implementation, this preserves all content
    /// block types (thinking, tool_use, tool_result, etc.) as separate turn
    /// items so that `loadHistory` in the GUI can reconstruct the full
    /// conversation including process information.
    pub async fn seed_thread_from_messages(
        &self,
        thread_id: &str,
        messages: &[Message],
    ) -> Result<()> {
        // Session seeding writes turns/items and then advances the existing
        // thread pointer as one synchronous record transaction.
        let thread_mutation = self.store.thread_mutation.lock();
        let mut thread = self
            .store
            .load_thread(thread_id)
            .with_context(|| format!("Thread not found: {thread_id}"))?;
        let now = Utc::now();

        // Group messages into turns. A turn starts with a user message and
        // includes all subsequent assistant messages (which may contain
        // thinking, tool_use, tool_result blocks) until the next user message.
        let mut turns: Vec<TurnSeed> = Vec::new();
        let mut current_turn: Option<TurnSeed> = None;

        for msg in messages {
            match msg.role.as_str() {
                "user" => {
                    let mut user_text = String::new();
                    let mut tool_results = Vec::new();

                    for block in &msg.content {
                        match block {
                            ContentBlock::Text { text, .. } if !text.trim().is_empty() => {
                                if !user_text.is_empty() {
                                    user_text.push('\n');
                                }
                                user_text.push_str(text);
                            }
                            ContentBlock::ToolResult {
                                tool_use_id,
                                content,
                                is_error,
                                content_blocks,
                            } => {
                                tool_results.push(SeedItem::ToolResult {
                                    tool_use_id: tool_use_id.clone(),
                                    content: content.clone(),
                                    is_error: is_error.unwrap_or(false),
                                    content_blocks: content_blocks.clone(),
                                });
                            }
                            // Other block types in user messages are rare;
                            // skip them gracefully.
                            _ => {}
                        }
                    }

                    if !user_text.is_empty() {
                        // A real user prompt begins a new turn. Tool results
                        // without text belong to the preceding assistant turn.
                        if let Some(t) = current_turn.take() {
                            turns.push(t);
                        }
                        current_turn = Some(TurnSeed {
                            user_text,
                            items: tool_results,
                        });
                    } else if !tool_results.is_empty() {
                        let turn = current_turn.get_or_insert_with(|| TurnSeed {
                            user_text: String::new(),
                            items: Vec::new(),
                        });
                        turn.items.extend(tool_results);
                    } else {
                        if let Some(t) = current_turn.take() {
                            turns.push(t);
                        }
                        current_turn = Some(TurnSeed {
                            user_text: String::new(),
                            items: Vec::new(),
                        });
                    }
                }
                "assistant" => {
                    // If no current turn exists (e.g. session starts with
                    // an assistant message), create a placeholder turn.
                    let turn = current_turn.get_or_insert_with(|| TurnSeed {
                        user_text: String::new(),
                        items: Vec::new(),
                    });
                    for block in &msg.content {
                        match block {
                            ContentBlock::Text { text, .. } if !text.trim().is_empty() => {
                                turn.items.push(SeedItem::Text(text.clone()));
                            }
                            ContentBlock::Thinking { thinking, .. }
                                if !thinking.trim().is_empty() =>
                            {
                                turn.items.push(SeedItem::Thinking(thinking.clone()));
                            }
                            ContentBlock::ToolUse {
                                id, name, input, ..
                            } => {
                                turn.items.push(SeedItem::ToolUse {
                                    id: id.clone(),
                                    name: name.clone(),
                                    input: input.clone(),
                                });
                            }
                            ContentBlock::ServerToolUse {
                                id, name, input, ..
                            } => {
                                turn.items.push(SeedItem::ToolUse {
                                    id: id.clone(),
                                    name: name.clone(),
                                    input: input.clone(),
                                });
                            }
                            // Skip other block types (image_url, etc.)
                            _ => {}
                        }
                    }
                }
                // System messages and other roles are ignored for turn seeding.
                _ => {}
            }
        }
        // Flush the last turn.
        if let Some(t) = current_turn.take() {
            turns.push(t);
        }

        for turn_seed in turns {
            let turn_id = format!("turn_{}", &Uuid::new_v4().to_string()[..8]);
            let summary =
                crate::utils::truncate_with_ellipsis(&turn_seed.user_text, SUMMARY_LIMIT, "...");
            let mut item_ids = Vec::new();

            // Save user message item.
            if !turn_seed.user_text.is_empty() {
                let item_id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
                self.store.save_item(&TurnItemRecord {
                    schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                    id: item_id.clone(),
                    turn_id: turn_id.clone(),
                    kind: TurnItemKind::UserMessage,
                    status: TurnItemLifecycleStatus::Completed,
                    summary: summary.clone(),
                    detail: Some(turn_seed.user_text.clone()),
                    metadata: None,
                    artifact_refs: Vec::new(),
                    started_at: Some(now),
                    ended_at: Some(now),
                })?;
                item_ids.push(item_id);
            }

            // Save assistant content items in order.
            for seed_item in &turn_seed.items {
                let item_id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
                match seed_item {
                    SeedItem::Text(text) => {
                        let asst_summary = if text.len() > SUMMARY_LIMIT {
                            crate::utils::truncate_with_ellipsis(text, SUMMARY_LIMIT, "...")
                        } else {
                            text.clone()
                        };
                        self.store.save_item(&TurnItemRecord {
                            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                            id: item_id.clone(),
                            turn_id: turn_id.clone(),
                            kind: TurnItemKind::AgentMessage,
                            status: TurnItemLifecycleStatus::Completed,
                            summary: asst_summary,
                            detail: Some(text.clone()),
                            metadata: None,
                            artifact_refs: Vec::new(),
                            started_at: Some(now),
                            ended_at: Some(now),
                        })?;
                    }
                    SeedItem::Thinking(thinking) => {
                        let thinking_summary = if thinking.len() > SUMMARY_LIMIT {
                            crate::utils::truncate_with_ellipsis(thinking, SUMMARY_LIMIT, "...")
                        } else {
                            thinking.clone()
                        };
                        self.store.save_item(&TurnItemRecord {
                            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                            id: item_id.clone(),
                            turn_id: turn_id.clone(),
                            kind: TurnItemKind::AgentReasoning,
                            status: TurnItemLifecycleStatus::Completed,
                            summary: thinking_summary,
                            detail: Some(thinking.clone()),
                            metadata: None,
                            artifact_refs: Vec::new(),
                            started_at: Some(now),
                            ended_at: Some(now),
                        })?;
                    }
                    SeedItem::ToolUse {
                        id: tool_id,
                        name,
                        input,
                    } => {
                        let input_str =
                            serde_json::to_string(input).unwrap_or_else(|_| input.to_string());
                        let tool_summary = format!("{name}({})", {
                            let s = &input_str;
                            if s.len() > 80 {
                                crate::utils::truncate_with_ellipsis(s, 80, "...")
                            } else {
                                s.clone()
                            }
                        });
                        self.store.save_item(&TurnItemRecord {
                            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                            id: item_id.clone(),
                            turn_id: turn_id.clone(),
                            kind: TurnItemKind::ToolCall,
                            status: TurnItemLifecycleStatus::Completed,
                            summary: tool_summary,
                            detail: Some(input_str),
                            metadata: Some(serde_json::Value::Object(
                                serde_json::json!({
                                    "tool_use_id": tool_id,
                                    "tool_name": name,
                                })
                                .as_object()
                                .unwrap()
                                .clone(),
                            )),
                            artifact_refs: Vec::new(),
                            started_at: Some(now),
                            ended_at: Some(now),
                        })?;
                    }
                    SeedItem::ToolResult {
                        tool_use_id,
                        content,
                        is_error,
                        content_blocks,
                    } => {
                        let result_summary = if content.len() > SUMMARY_LIMIT {
                            crate::utils::truncate_with_ellipsis(content, SUMMARY_LIMIT, "...")
                        } else {
                            content.clone()
                        };
                        let mut metadata = serde_json::Map::new();
                        metadata.insert("tool_result_for".to_string(), json!(tool_use_id));
                        metadata.insert("is_error".to_string(), json!(is_error));
                        if let Some(blocks) = content_blocks {
                            metadata
                                .insert("content_blocks".to_string(), Value::Array(blocks.clone()));
                        }
                        self.store.save_item(&TurnItemRecord {
                            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                            id: item_id.clone(),
                            turn_id: turn_id.clone(),
                            kind: TurnItemKind::ToolCall,
                            status: if *is_error {
                                TurnItemLifecycleStatus::Failed
                            } else {
                                TurnItemLifecycleStatus::Completed
                            },
                            summary: result_summary,
                            detail: Some(content.clone()),
                            metadata: Some(Value::Object(metadata)),
                            artifact_refs: Vec::new(),
                            started_at: Some(now),
                            ended_at: Some(now),
                        })?;
                    }
                }
                item_ids.push(item_id);
            }

            // Only create a turn if there's content.
            if !item_ids.is_empty() {
                self.store.save_turn(&TurnRecord {
                    schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                    id: turn_id.clone(),
                    thread_id: thread_id.to_string(),
                    status: RuntimeTurnStatus::Completed,
                    input_summary: summary,
                    created_at: now,
                    started_at: Some(now),
                    ended_at: Some(now),
                    duration_ms: Some(0),
                    usage: None,
                    effective_provider: None,
                    effective_provider_id: None,
                    effective_billing_surface: None,
                    effective_model: None,
                    error: None,
                    item_ids,
                    steer_count: 0,
                })?;

                thread.latest_turn_id = Some(turn_id);
                thread.updated_at = now;
            }
        }

        self.store.save_thread(&thread)?;
        drop(thread_mutation);
        self.emit_event(
            thread_id,
            None,
            None,
            "thread.updated",
            json!({ "thread": thread, "reason": "session_resume" }),
        )
        .await?;
        Ok(())
    }

    fn cleanup_unaccepted_turn_records(&self, turn_id: &str, item_id: Option<&str>) -> Result<()> {
        let mut errors = Vec::new();
        if let Some(item_id) = item_id
            && let Err(err) = self.store.remove_item(item_id)
        {
            errors.push(format!("remove item: {err}"));
        }
        if let Err(err) = self.store.remove_turn(turn_id) {
            errors.push(format!("remove turn: {err}"));
        }
        if errors.is_empty() {
            Ok(())
        } else {
            bail!(errors.join("; "))
        }
    }

    async fn emit_claimed_turn_started(
        &self,
        turn: &TurnRecord,
        user_item: Option<&TurnItemRecord>,
        kind: ClaimedTurnKind,
    ) {
        let start_payload = match kind {
            ClaimedTurnKind::Message => json!({ "turn": turn.clone() }),
            ClaimedTurnKind::Compaction => {
                json!({ "turn": turn.clone(), "manual_compaction": true })
            }
        };
        if let Err(err) = self
            .emit_event(
                &turn.thread_id,
                Some(&turn.id),
                None,
                "turn.started",
                start_payload,
            )
            .await
        {
            tracing::warn!(
                "Failed to persist {}.started after engine acceptance: {err}",
                kind.label()
            );
        }

        if let Some(user_item) = user_item {
            if let Err(err) = self
                .emit_event(
                    &turn.thread_id,
                    Some(&turn.id),
                    Some(&user_item.id),
                    "item.started",
                    json!({ "item": user_item.clone() }),
                )
                .await
            {
                tracing::warn!("Failed to persist item.started after engine acceptance: {err}");
            }
            if let Err(err) = self
                .emit_event(
                    &turn.thread_id,
                    Some(&turn.id),
                    Some(&user_item.id),
                    "item.completed",
                    json!({ "item": user_item.clone() }),
                )
                .await
            {
                tracing::warn!("Failed to persist item.completed after engine acceptance: {err}");
            }
        }
    }

    async fn settle_claimed_turn_failure(&self, thread_id: &str, turn_id: &str, reason: &str) {
        // Block steer attempts while terminal receipts are being settled; the
        // active claim remains present so a replacement turn cannot start.
        {
            let mut active = self.active.lock().await;
            if let Some(turn) = active
                .engines
                .get_mut(thread_id)
                .and_then(|state| state.active_turn.as_mut())
                && turn.turn_id == turn_id
            {
                turn.interrupt_requested = true;
            }
        }
        let now = Utc::now();
        let mut terminal_items = Vec::new();
        match self.store.list_items_for_turn(turn_id) {
            Ok(items) => {
                for mut item in items {
                    if matches!(
                        item.status,
                        TurnItemLifecycleStatus::Queued | TurnItemLifecycleStatus::InProgress
                    ) {
                        item.status = TurnItemLifecycleStatus::Failed;
                        item.ended_at = Some(now);
                        match self.store.save_item(&item) {
                            Ok(()) => terminal_items.push(item),
                            Err(err) => tracing::error!(
                                item_id = %item.id,
                                "Failed to terminalize item after monitor failure: {err}"
                            ),
                        }
                    }
                }
            }
            Err(err) => tracing::error!(
                "Failed to list turn items after monitor failure for {turn_id}: {err}"
            ),
        }
        let terminal_turn = {
            let _turn_mutation = self.store.turn_mutation.lock();
            match self.store.load_turn(turn_id) {
                Ok(mut turn) if turn.status == RuntimeTurnStatus::InProgress => {
                    turn.status = RuntimeTurnStatus::Failed;
                    turn.ended_at = Some(now);
                    turn.duration_ms = turn.started_at.map(|start| duration_ms(start, now));
                    turn.error = Some(reason.to_string());
                    match self.store.save_turn(&turn) {
                        Ok(()) => Some(turn),
                        Err(err) => {
                            tracing::error!("Failed to persist terminal monitor failure: {err}");
                            None
                        }
                    }
                }
                Ok(_) => None,
                Err(err) => {
                    tracing::error!("Failed to load turn after monitor failure: {err}");
                    None
                }
            }
        };

        for item in terminal_items {
            if let Err(err) = self
                .emit_event(
                    thread_id,
                    Some(turn_id),
                    Some(&item.id),
                    "item.failed",
                    json!({ "item": item, "error": reason }),
                )
                .await
            {
                tracing::error!("Failed to emit terminal item failure: {err}");
            }
        }

        if let Some(turn) = terminal_turn
            && let Err(err) = self
                .emit_event(
                    thread_id,
                    Some(turn_id),
                    None,
                    "turn.completed",
                    json!({ "turn": turn }),
                )
                .await
        {
            tracing::error!("Failed to emit terminal monitor failure: {err}");
        }

        // Keep the failed claim in place until its terminal receipts are
        // ordered. Then poison and evict this engine so the next turn gets a
        // distinct event receiver and cannot consume stale terminal events.
        let evicted_engine = {
            let mut active = self.active.lock().await;
            let owns_failed_turn = active
                .engines
                .get(thread_id)
                .and_then(|state| state.active_turn.as_ref())
                .is_some_and(|turn| turn.turn_id == turn_id);
            if owns_failed_turn {
                active.lru.retain(|id| id != thread_id);
                active.engines.remove(thread_id).map(|state| state.engine)
            } else {
                None
            }
        };
        if let Some(engine) = evicted_engine {
            engine.cancel_with_reason(crate::core::engine::CancelReason::Internal);
            let _ = engine.try_send(Op::Shutdown);
        }
    }

    async fn monitor_claimed_turn(
        &self,
        thread_id: String,
        turn_id: String,
        engine: EngineHandle,
        kind: ClaimedTurnKind,
    ) {
        if self.cancel_token.is_cancelled() {
            engine.cancel_with_reason(crate::core::engine::CancelReason::Internal);
            self.settle_claimed_turn_failure(
                &thread_id,
                &turn_id,
                "Runtime shutdown requested before turn monitoring started",
            )
            .await;
            return;
        }

        use futures_util::FutureExt;
        let result = std::panic::AssertUnwindSafe(self.monitor_turn(
            thread_id.clone(),
            turn_id.clone(),
            engine.clone(),
        ))
        .catch_unwind()
        .await;
        let failure = match result {
            Ok(Ok(())) => return,
            Ok(Err(error)) => format!("Failed to monitor {}: {error}", kind.label()),
            Err(payload) => format!(
                "{} monitor panicked: {}",
                kind.label(),
                panic_payload_message(&*payload)
            ),
        };
        tracing::error!("{failure}");
        engine.cancel_with_reason(crate::core::engine::CancelReason::Internal);
        self.settle_claimed_turn_failure(&thread_id, &turn_id, &failure)
            .await;
    }

    fn spawn_claimed_turn_monitor(
        &self,
        turn: TurnRecord,
        user_item: Option<TurnItemRecord>,
        engine: EngineHandle,
        kind: ClaimedTurnKind,
    ) -> oneshot::Receiver<std::result::Result<TurnRecord, String>> {
        let (acceptance_tx, acceptance_rx) = oneshot::channel();
        let manager = Arc::new(self.clone());
        tokio::spawn(async move {
            use futures_util::FutureExt;
            let start_events = std::panic::AssertUnwindSafe(manager.emit_claimed_turn_started(
                &turn,
                user_item.as_ref(),
                kind,
            ))
            .catch_unwind()
            .await;
            if let Err(payload) = start_events {
                let failure = format!(
                    "{} start-event recording panicked after engine acceptance: {}",
                    kind.label(),
                    panic_payload_message(&*payload)
                );
                tracing::error!("{failure}");
                let _ = acceptance_tx.send(Ok(turn.clone()));
                engine.cancel_with_reason(crate::core::engine::CancelReason::Internal);
                manager
                    .settle_claimed_turn_failure(&turn.thread_id, &turn.id, &failure)
                    .await;
                return;
            }

            let _ = acceptance_tx.send(Ok(turn.clone()));
            manager
                .monitor_claimed_turn(turn.thread_id.clone(), turn.id.clone(), engine, kind)
                .await;
        });
        acceptance_rx
    }

    fn spawn_steer_receipts(
        &self,
        turn: TurnRecord,
        item: TurnItemRecord,
        prompt: String,
    ) -> oneshot::Receiver<TurnRecord> {
        let (receipt_tx, receipt_rx) = oneshot::channel();
        let manager = Arc::new(self.clone());
        tokio::spawn(async move {
            use futures_util::FutureExt;
            let receipts = std::panic::AssertUnwindSafe(async {
                if let Err(err) = manager
                    .emit_event(
                        &turn.thread_id,
                        Some(&turn.id),
                        Some(&item.id),
                        "turn.steered",
                        json!({
                            "thread_id": turn.thread_id.clone(),
                            "turn_id": turn.id.clone(),
                            "input": prompt,
                        }),
                    )
                    .await
                {
                    tracing::warn!("Failed to persist turn.steered after engine acceptance: {err}");
                }
                if let Err(err) = manager
                    .emit_event(
                        &turn.thread_id,
                        Some(&turn.id),
                        Some(&item.id),
                        "item.completed",
                        json!({ "item": item }),
                    )
                    .await
                {
                    tracing::warn!("Failed to persist steer item.completed: {err}");
                }
            })
            .catch_unwind()
            .await;
            if let Err(payload) = receipts {
                tracing::error!(
                    "Steer receipt task panicked after engine acceptance: {}",
                    panic_payload_message(&*payload)
                );
            }
            let _ = receipt_tx.send(turn);
        });
        receipt_rx
    }

    pub async fn start_turn(&self, thread_id: &str, req: StartTurnRequest) -> Result<TurnRecord> {
        let prompt = req.prompt.trim().to_string();
        if prompt.is_empty() {
            bail!("prompt is required");
        }

        let thread = self.get_thread(thread_id).await?;
        let engine = self.ensure_engine_loaded(&thread).await?;

        let client_preflight_required = {
            let active = self.active.lock().await;
            if let Some(active_thread) = active.engines.get(thread_id)
                && active_thread.active_turn.is_some()
            {
                bail!("Thread already has an active turn");
            }
            active
                .engines
                .get(thread_id)
                .is_none_or(|state| state.client_preflight_required)
        };

        // Resolve the concrete provider/model before persisting a turn. Auto
        // routing can fail, and such a failure must not leave a zombie
        // in-progress record behind.
        let mode = req
            .mode
            .as_deref()
            .and_then(parse_mode_opt)
            .unwrap_or_else(|| parse_mode(&thread.mode));
        let requested_model = req.model.as_deref().unwrap_or(&thread.model).to_string();
        let auto_model = requested_model.trim().eq_ignore_ascii_case("auto");
        let cfg_snapshot = self.config.read().clone();
        let identity = self.provider_identity_for_thread(&cfg_snapshot, &thread)?;
        let mut thread_config = cfg_snapshot.clone();
        thread_config.scope_to_provider_identity(&identity);
        let verbosity = thread_config.verbosity.clone();
        let (route, reasoning_effort) = if auto_model {
            let selection = crate::model_routing::resolve_auto_route_with_inventory(
                &thread_config,
                &prompt,
                "",
                "auto",
                "auto",
            )
            .await?;
            let route = resolve_runtime_thread_route(
                &thread_config,
                selection.provider,
                Some(&selection.model),
            )?;
            (
                route,
                selection
                    .reasoning_effort
                    .map(|effort| effort.as_setting().to_string()),
            )
        } else {
            (
                resolve_runtime_thread_route_for_identity(
                    &cfg_snapshot,
                    &identity,
                    Some(&requested_model),
                )?,
                None,
            )
        };
        let route = if client_preflight_required {
            route
                .preflight()
                .map_err(|reason| anyhow!("Failed to validate runtime thread route: {reason}"))?
        } else {
            route
        };
        let provider = route.identity.provider;
        let provider_identity = route.identity.clone();
        let model = route.model.clone();
        let route_limits = known_route_limits(route.candidate.limits);
        let settings = crate::settings::Settings::load().unwrap_or_default();
        let compaction = runtime_compaction_config(
            provider,
            &model,
            route_limits,
            settings.auto_compact,
            crate::settings::Settings::auto_compact_explicitly_configured(),
            settings.auto_compact_threshold_percent,
        );
        let show_thinking = settings.show_thinking;

        let now = Utc::now();
        let turn_id = format!("turn_{}", &Uuid::new_v4().to_string()[..8]);
        let mut turn = TurnRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            id: turn_id.clone(),
            thread_id: thread_id.to_string(),
            status: RuntimeTurnStatus::InProgress,
            input_summary: req
                .input_summary
                .unwrap_or_else(|| summarize_text(&prompt, SUMMARY_LIMIT)),
            created_at: now,
            started_at: Some(now),
            ended_at: None,
            duration_ms: None,
            usage: None,
            effective_provider: Some(provider.as_str().to_string()),
            effective_provider_id: provider_identity.exact_id.clone(),
            effective_billing_surface: None,
            effective_model: Some(model.clone()),
            error: None,
            item_ids: Vec::new(),
            steer_count: 0,
        };

        let user_item_id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
        let user_item = TurnItemRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            id: user_item_id.clone(),
            turn_id: turn_id.clone(),
            kind: TurnItemKind::UserMessage,
            status: TurnItemLifecycleStatus::Completed,
            summary: summarize_text(&prompt, SUMMARY_LIMIT),
            detail: Some(prompt.clone()),
            metadata: None,
            artifact_refs: Vec::new(),
            started_at: Some(now),
            ended_at: Some(now),
        };
        turn.item_ids.push(user_item_id.clone());

        let allow_shell = req.allow_shell.unwrap_or(thread.allow_shell);
        let trust_mode = req.trust_mode.unwrap_or(thread.trust_mode);
        let auto_approve = req.auto_approve.unwrap_or(thread.auto_approve);
        let op = Op::SendMessage {
            content: prompt,
            input: None,
            mode,
            route: Box::new(route),
            compaction: Box::new(compaction),
            goal_objective: None,
            goal_token_budget: None,
            goal_status: crate::tools::goal::GoalStatus::Active,
            reasoning_effort,
            reasoning_effort_auto: auto_model,
            auto_model,
            allow_shell,
            trust_mode,
            auto_approve,
            translation_enabled: false,
            show_thinking,
            allowed_tools: None,
            dynamic_tools: req.dynamic_tools,
            hook_executor: None,
            approval_mode: if auto_approve {
                crate::tui::approval::ApprovalMode::Bypass
            } else {
                crate::tui::approval::ApprovalMode::Suggest
            },
            verbosity,
            provenance: crate::core::ops::UserInputProvenance::ExternalUser,
        };

        // Reserve mailbox capacity before claiming or persisting anything.
        // If the caller is cancelled while capacity is unavailable, no
        // durable or in-memory turn state has changed.
        let permit = engine
            .tx_op
            .clone()
            .reserve_owned()
            .await
            .map_err(|_| anyhow!("Failed to start turn: engine operation channel closed"))?;

        let acceptance_rx = {
            // Lock order is active -> thread_mutation. Neither guard crosses
            // an await, and spawning the owned lifecycle task is synchronous.
            let mut active = self.active.lock().await;
            let Some(state) = active.engines.get_mut(thread_id) else {
                bail!("Thread engine not loaded");
            };
            if state.active_turn.is_some() {
                bail!("Thread already has an active turn");
            }
            let _thread_mutation = self.store.thread_mutation.lock();
            let mut current_thread = self.store.load_thread(thread_id)?;
            if !thread_execution_state_matches(&thread, &current_thread) {
                bail!("Thread execution settings changed while preparing the turn; retry");
            }
            let previous_active_route = (state.route_identity.clone(), state.route_model.clone());
            state.active_turn = Some(ActiveTurnState {
                turn_id: turn_id.clone(),
                interrupt_requested: false,
                auto_approve,
                trust_mode,
            });
            state.route_identity = provider_identity;
            state.route_model.clone_from(&model);

            let persistence_result = (|| -> Result<()> {
                self.store.save_item(&user_item)?;
                self.store.save_turn(&turn)?;
                current_thread.latest_turn_id = Some(turn_id.clone());
                current_thread.updated_at = now;
                self.store.save_thread(&current_thread)
            })();
            if let Err(persistence_error) = persistence_result {
                let cleanup_error = self
                    .cleanup_unaccepted_turn_records(&turn_id, Some(&user_item_id))
                    .err();
                state.active_turn = None;
                state.route_identity = previous_active_route.0;
                state.route_model = previous_active_route.1;
                return match cleanup_error {
                    None => Err(anyhow!("Failed to persist turn: {persistence_error}")),
                    Some(cleanup_error) => Err(anyhow!(
                        "Failed to persist turn: {persistence_error}; cleanup also failed: {cleanup_error}"
                    )),
                };
            }

            // Sending through an owned permit cannot await or fail. From this
            // point the engine owns the operation and the spawned task owns
            // lifecycle events, monitoring, and terminal cleanup even if the
            // HTTP/client future is dropped.
            let _sender = permit.send(op);
            touch_lru(&mut active.lru, thread_id);
            self.spawn_claimed_turn_monitor(
                turn.clone(),
                Some(user_item),
                engine.clone(),
                ClaimedTurnKind::Message,
            )
        };

        acceptance_rx
            .await
            .map_err(|_| anyhow!("Turn lifecycle task ended before acknowledgement"))?
            .map_err(anyhow::Error::msg)
    }

    pub async fn interrupt_turn(&self, thread_id: &str, turn_id: &str) -> Result<TurnRecord> {
        {
            let mut active = self.active.lock().await;
            let Some(active_thread) = active.engines.get_mut(thread_id) else {
                bail!("Thread is not loaded");
            };
            let Some(active_turn) = active_thread.active_turn.as_mut() else {
                bail!("No active turn on thread {thread_id}");
            };
            if active_turn.turn_id != turn_id {
                bail!("Turn {turn_id} is not active on thread {thread_id}");
            }
            active_turn.interrupt_requested = true;
            active_thread.engine.cancel();
            touch_lru(&mut active.lru, thread_id);
        }

        self.emit_event(
            thread_id,
            Some(turn_id),
            None,
            "turn.interrupt_requested",
            json!({ "thread_id": thread_id, "turn_id": turn_id }),
        )
        .await?;

        self.store.load_turn(turn_id)
    }

    pub async fn steer_turn(
        &self,
        thread_id: &str,
        turn_id: &str,
        req: SteerTurnRequest,
    ) -> Result<TurnRecord> {
        let prompt = req.prompt.trim().to_string();
        if prompt.is_empty() {
            bail!("prompt is required");
        }

        let engine = {
            let mut active = self.active.lock().await;
            let engine = {
                let Some(active_thread) = active.engines.get_mut(thread_id) else {
                    bail!("Thread is not loaded");
                };
                let Some(active_turn) = active_thread.active_turn.as_mut() else {
                    bail!("No active turn on thread {thread_id}");
                };
                if active_turn.turn_id != turn_id {
                    bail!("Turn {turn_id} is not active on thread {thread_id}");
                }
                if active_turn.interrupt_requested {
                    bail!("Turn {turn_id} is stopping and cannot be steered");
                }
                active_thread.engine.clone()
            };
            touch_lru(&mut active.lru, thread_id);
            engine
        };

        let permit = engine
            .reserve_steer()
            .await
            .map_err(|error| anyhow!("Failed to steer turn: {error}"))?;

        let now = Utc::now();
        let item = TurnItemRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            id: format!("item_{}", &Uuid::new_v4().to_string()[..8]),
            turn_id: turn_id.to_string(),
            kind: TurnItemKind::UserMessage,
            status: TurnItemLifecycleStatus::Completed,
            summary: summarize_text(&prompt, SUMMARY_LIMIT),
            detail: Some(prompt.clone()),
            metadata: None,
            artifact_refs: Vec::new(),
            started_at: Some(now),
            ended_at: Some(now),
        };
        let receipt_rx = {
            let mut active = self.active.lock().await;
            let Some(active_thread) = active.engines.get(thread_id) else {
                bail!("Thread is not loaded");
            };
            let Some(active_turn) = active_thread.active_turn.as_ref() else {
                bail!("No active turn on thread {thread_id}");
            };
            if active_turn.turn_id != turn_id {
                bail!("Turn {turn_id} is not active on thread {thread_id}");
            }
            if active_turn.interrupt_requested {
                bail!("Turn {turn_id} is stopping and cannot be steered");
            }
            if !active_thread.engine.tx_op.same_channel(&engine.tx_op) {
                bail!("Thread engine changed while preparing steer; retry");
            }
            let _turn_mutation = self.store.turn_mutation.lock();
            let persistence = (|| -> Result<TurnRecord> {
                let mut turn = self.store.load_turn(turn_id)?;
                if turn.status != RuntimeTurnStatus::InProgress {
                    bail!("Turn {turn_id} is no longer in progress and cannot be steered");
                }
                self.store.save_item(&item)?;
                turn.steer_count = turn.steer_count.saturating_add(1);
                if !turn.item_ids.iter().any(|id| id == &item.id) {
                    turn.item_ids.push(item.id.clone());
                }
                self.store.save_turn(&turn)?;
                Ok(turn)
            })();
            let turn = match persistence {
                Ok(turn) => turn,
                Err(error) => {
                    let cleanup = self.store.remove_item(&item.id);
                    return match cleanup {
                        Ok(()) => Err(error),
                        Err(cleanup_error) => Err(anyhow!(
                            "Failed to persist steer: {error}; cleanup also failed: {cleanup_error}"
                        )),
                    };
                }
            };
            // The reserved send has no await/failure point. From here the
            // engine and durable record agree even if the API caller drops.
            let _sender = permit.send(prompt.clone());
            touch_lru(&mut active.lru, thread_id);
            self.spawn_steer_receipts(turn, item, prompt)
        };
        receipt_rx
            .await
            .map_err(|_| anyhow!("Steer receipt task ended before acknowledgement"))
    }

    pub async fn compact_thread(
        &self,
        thread_id: &str,
        req: CompactThreadRequest,
    ) -> Result<TurnRecord> {
        let thread = self.get_thread(thread_id).await?;
        let engine = self.ensure_engine_loaded(&thread).await?;

        let client_preflight_required = {
            let active = self.active.lock().await;
            let Some(active_thread) = active.engines.get(thread_id) else {
                bail!("Thread engine not loaded");
            };
            if active_thread.active_turn.is_some() {
                bail!("Thread already has an active turn");
            }
            active_thread.client_preflight_required
        };
        let route = self.resolved_route_for_thread(&self.read_config(), &thread)?;
        let route = if client_preflight_required {
            route
                .preflight()
                .map_err(|reason| anyhow!("Failed to validate runtime thread route: {reason}"))?
        } else {
            route
        };
        let route_provider = route.identity.provider;
        let route_identity = route.identity.clone();
        let route_model = route.model.clone();
        let route_limits = known_route_limits(route.candidate.limits);
        let settings = crate::settings::Settings::load().unwrap_or_default();
        let compaction = runtime_compaction_config(
            route_provider,
            &route_model,
            route_limits,
            settings.auto_compact,
            crate::settings::Settings::auto_compact_explicitly_configured(),
            settings.auto_compact_threshold_percent,
        );

        let now = Utc::now();
        let turn_id = format!("turn_{}", &Uuid::new_v4().to_string()[..8]);
        let turn = TurnRecord {
            schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
            id: turn_id.clone(),
            thread_id: thread_id.to_string(),
            status: RuntimeTurnStatus::InProgress,
            input_summary: req
                .reason
                .as_deref()
                .map(|s| summarize_text(s, SUMMARY_LIMIT))
                .unwrap_or_else(|| "Manual context compaction".to_string()),
            created_at: now,
            started_at: Some(now),
            ended_at: None,
            duration_ms: None,
            usage: None,
            effective_provider: Some(route_provider.as_str().to_string()),
            effective_provider_id: route_identity.exact_id.clone(),
            effective_billing_surface: None,
            effective_model: Some(route_model.clone()),
            error: None,
            item_ids: Vec::new(),
            steer_count: 0,
        };
        let op = Op::CompactContext {
            route: Box::new(route),
            compaction: Box::new(compaction),
        };
        let permit = engine.tx_op.clone().reserve_owned().await.map_err(|_| {
            anyhow!("Failed to trigger compaction: engine operation channel closed")
        })?;

        let acceptance_rx = {
            let mut active = self.active.lock().await;
            let Some(state) = active.engines.get_mut(thread_id) else {
                bail!("Thread engine not loaded");
            };
            if state.active_turn.is_some() {
                bail!("Thread already has an active turn");
            }
            let _thread_mutation = self.store.thread_mutation.lock();
            let mut current_thread = self.store.load_thread(thread_id)?;
            if !thread_execution_state_matches(&thread, &current_thread) {
                bail!("Thread execution settings changed while preparing compaction; retry");
            }
            let previous_active_route = (state.route_identity.clone(), state.route_model.clone());
            state.active_turn = Some(ActiveTurnState {
                turn_id: turn_id.clone(),
                interrupt_requested: false,
                auto_approve: current_thread.auto_approve,
                trust_mode: current_thread.trust_mode,
            });
            state.route_identity = route_identity;
            state.route_model = route_model;

            let persistence_result = (|| -> Result<()> {
                self.store.save_turn(&turn)?;
                current_thread.latest_turn_id = Some(turn_id.clone());
                current_thread.updated_at = now;
                self.store.save_thread(&current_thread)
            })();
            if let Err(persistence_error) = persistence_result {
                let cleanup_error = self.cleanup_unaccepted_turn_records(&turn_id, None).err();
                state.active_turn = None;
                state.route_identity = previous_active_route.0;
                state.route_model = previous_active_route.1;
                return match cleanup_error {
                    None => Err(anyhow!("Failed to persist compaction: {persistence_error}")),
                    Some(cleanup_error) => Err(anyhow!(
                        "Failed to persist compaction: {persistence_error}; cleanup also failed: {cleanup_error}"
                    )),
                };
            }

            let _sender = permit.send(op);
            touch_lru(&mut active.lru, thread_id);
            self.spawn_claimed_turn_monitor(
                turn.clone(),
                None,
                engine.clone(),
                ClaimedTurnKind::Compaction,
            )
        };

        acceptance_rx
            .await
            .map_err(|_| anyhow!("Compaction lifecycle task ended before acknowledgement"))?
            .map_err(anyhow::Error::msg)
    }

    pub fn events_since(
        &self,
        thread_id: &str,
        since_seq: Option<u64>,
    ) -> Result<Vec<RuntimeEventRecord>> {
        self.store.events_since(thread_id, since_seq)
    }

    async fn ensure_engine_loaded(&self, thread_hint: &ThreadRecord) -> Result<EngineHandle> {
        {
            let mut active = self.active.lock().await;
            if let Some(engine) = active
                .engines
                .get(thread_hint.id.as_str())
                .map(|state| state.engine.clone())
            {
                touch_lru(&mut active.lru, &thread_hint.id);
                return Ok(engine);
            }
        }

        // Only one cache-miss build may run at a time. Recheck after taking
        // the build lock because another caller may already have won.
        let _engine_load = self.engine_load.lock().await;
        loop {
            {
                let mut active = self.active.lock().await;
                if let Some(engine) = active
                    .engines
                    .get(thread_hint.id.as_str())
                    .map(|state| state.engine.clone())
                {
                    touch_lru(&mut active.lru, &thread_hint.id);
                    return Ok(engine);
                }
            }
            let thread = {
                let _thread_mutation = self.store.thread_mutation.lock();
                self.store
                    .load_thread(&thread_hint.id)
                    .with_context(|| format!("Thread not found: {}", thread_hint.id))?
            };

            // Snapshot and prepare the concrete provider route once so the engine,
            // route limits, compaction budget, and restored session all agree.
            let base_config = self.read_config().clone();
            let route = self.resolved_route_for_thread(&base_config, &thread)?;
            let provider = route.identity.provider;
            let route_identity = route.identity;
            let route_model = route.model;
            let route_limits = known_route_limits(route.candidate.limits);
            let cfg = route.config;

            // Resolve the provider-route-aware auto-compaction default unless the
            // user persisted an explicit preference.
            let settings = crate::settings::Settings::load().unwrap_or_default();
            let compaction = runtime_compaction_config(
                provider,
                &route_model,
                route_limits,
                settings.auto_compact,
                crate::settings::Settings::auto_compact_explicitly_configured(),
                settings.auto_compact_threshold_percent,
            );
            let network_policy = cfg.network.clone().map(|toml_cfg| {
                crate::network_policy::NetworkPolicyDecider::with_default_audit(
                    toml_cfg.into_runtime(),
                )
            });
            let lsp_config = cfg
                .lsp
                .clone()
                .map(crate::config::LspConfigToml::into_runtime);
            let max_subagents = cfg
                .max_subagents_for_provider(provider)
                .clamp(1, MAX_SUBAGENTS);
            let engine_cfg = EngineConfig {
                model: route_model.clone(),
                active_route_limits: route_limits,
                workspace: thread.workspace.clone(),
                allow_shell: thread.allow_shell,
                trust_mode: thread.trust_mode,
                tool_whitelist: None,
                reasoning_effort: None,
                extra_tools: crate::core::engine::ExtraTools::default(),
                notes_path: cfg.notes_path(),
                mcp_config_path: cfg.mcp_config_path(),
                skills_dir: cfg.skills_dir(),
                skills_scan_codewhale_only: cfg.skills_config().scan_codewhale_only(),
                instructions: cfg
                    .instructions_paths()
                    .into_iter()
                    .map(Into::into)
                    .collect(),
                project_context_pack_enabled: cfg.project_context_pack_enabled(),
                translation_enabled: false,
                show_thinking: settings.show_thinking,
                max_steps: 100,
                max_subagents,
                max_admitted_subagents: cfg
                    .max_admitted_subagents_for_provider(provider)
                    .max(max_subagents),
                launch_concurrency: cfg.launch_concurrency_for_provider(provider),
                subagents_enabled: cfg.subagents_enabled_for_provider(provider),
                features: cfg.features(),
                auto_review_policy: cfg.auto_review_policy(),
                compaction,
                todos: new_shared_todo_list(),
                plan_state: new_shared_plan_state(),
                goal_state: crate::tools::goal::new_shared_goal_state(),
                max_spawn_depth: cfg.subagent_max_spawn_depth_for_provider(provider),
                subagent_token_budget: cfg.subagent_token_budget_for_provider(provider),
                network_policy,
                snapshots_enabled: cfg.snapshots_config().enabled,
                snapshots_max_workspace_bytes: cfg
                    .snapshots_config()
                    .max_workspace_gb
                    .saturating_mul(1024 * 1024 * 1024),
                lsp_config,
                runtime_services: crate::tools::spec::RuntimeToolServices {
                    task_manager: self.task_manager.lock().clone(),
                    automations: self.automations.lock().clone(),
                    task_data_dir: Some(self.manager_cfg.task_data_dir.clone()),
                    active_task_id: thread.task_id.clone(),
                    active_thread_id: Some(thread.id.clone()),
                    dynamic_tool_executor: Some(Arc::new(self.clone())),
                    shell_manager: None,
                    hook_executor: None,
                    handle_store: crate::tools::handle::new_shared_handle_store(),
                    rlm_sessions: crate::rlm::session::new_shared_rlm_session_store(),
                },
                subagent_model_overrides: cfg.subagent_model_overrides(),
                fleet_roster: Arc::new(crate::fleet::roster::FleetRoster::load(
                    &cfg.fleet_config(),
                    &thread.workspace,
                )),
                subagent_api_timeout: std::time::Duration::from_secs(
                    cfg.subagent_api_timeout_secs_for_provider(provider),
                ),
                stream_chunk_timeout: std::time::Duration::from_secs(
                    cfg.stream_chunk_timeout_secs(),
                ),
                subagent_heartbeat_timeout: std::time::Duration::from_secs(
                    cfg.subagent_heartbeat_timeout_secs_for_provider(provider),
                ),
                prefer_bwrap: cfg.prefer_bwrap.unwrap_or(false),
                memory_enabled: cfg.memory_enabled(),
                moraine_fallback: cfg.moraine_fallback(),
                memory_path: cfg.memory_path(),
                speech_output_dir: cfg.speech_output_dir(),
                vision_config: cfg.vision_model_config(),
                strict_tool_mode: cfg.strict_tool_mode.unwrap_or(false),
                goal_objective: None,
                goal_token_budget: None,
                goal_status: crate::tools::goal::GoalStatus::Active,
                allowed_tools: None,
                disallowed_tools: None,
                hook_executor: None,
                locale_tag: crate::localization::resolve_locale(&settings.locale)
                    .tag()
                    .to_string(),
                workshop: cfg.workshop.clone(),
                search_provider: cfg.search_provider(),
                search_api_key: cfg.search.as_ref().and_then(|s| s.api_key.clone()),
                search_base_url: cfg.search.as_ref().and_then(|s| s.base_url.clone()),
                tools_always_load: cfg.tools_always_load(),
                tools: cfg.tools.clone(),
                verbosity: cfg.verbosity.clone(),
                workspace_follow_symlinks: settings.workspace_follow_symlinks,
                exec_policy_engine: cfg.exec_policy_engine.clone(),
                terminal_chrome_enabled: false,
            };

            let engine = spawn_engine_with_authoritative_route_config(
                engine_cfg,
                &cfg,
                Arc::clone(&self.config),
            );

            // When the thread has an associated session, load the full message history
            // (including thinking/tool blocks) from the session file. This preserves
            // process information that `reconstruct_messages_from_turns` would lose.
            let session_messages = if let Some(ref sid) = thread.session_id {
                match crate::session_manager::default_sessions_dir() {
                    Ok(sessions_dir) => {
                        match crate::session_manager::SessionManager::new(sessions_dir) {
                            Ok(manager) => match manager.load_session(sid) {
                                Ok(session) => session.messages,
                                Err(e) => {
                                    tracing::warn!(
                                        "Failed to load session {} for thread {}: {e}; falling back to turn reconstruction",
                                        sid,
                                        thread.id
                                    );
                                    let turns = self.store.list_turns_for_thread(&thread.id)?;
                                    self.reconstruct_messages_from_turns(&turns)?
                                }
                            },
                            Err(e) => {
                                tracing::warn!(
                                    "Failed to open sessions dir: {e}; falling back to turn reconstruction"
                                );
                                let turns = self.store.list_turns_for_thread(&thread.id)?;
                                self.reconstruct_messages_from_turns(&turns)?
                            }
                        }
                    }
                    Err(e) => {
                        tracing::warn!(
                            "Failed to resolve sessions dir: {e}; falling back to turn reconstruction"
                        );
                        let turns = self.store.list_turns_for_thread(&thread.id)?;
                        self.reconstruct_messages_from_turns(&turns)?
                    }
                }
            } else {
                let turns = self.store.list_turns_for_thread(&thread.id)?;
                self.reconstruct_messages_from_turns(&turns)?
            };
            let sys_prompt = thread
                .system_prompt
                .as_ref()
                .map(|s| SystemPrompt::Text(s.clone()));
            if !session_messages.is_empty() || sys_prompt.is_some() {
                engine
                    .send(Op::SyncSession {
                        session_id: thread.session_id.clone(),
                        messages: session_messages,
                        system_prompt: sys_prompt,
                        system_prompt_override: thread.system_prompt.is_some(),
                        model: route_model.clone(),
                        workspace: thread.workspace.clone(),
                        mode: parse_mode(&thread.mode),
                    })
                    .await
                    .map_err(|e| anyhow!("Failed to sync thread session: {e}"))?;
            }

            let mut active = self.active.lock().await;
            if let Some(winner) = active
                .engines
                .get(&thread.id)
                .map(|state| state.engine.clone())
            {
                touch_lru(&mut active.lru, &thread.id);
                drop(active);
                engine.cancel_with_reason(crate::core::engine::CancelReason::Internal);
                let _ = engine.try_send(Op::Shutdown);
                return Ok(winner);
            }

            // Atomically compare the record used for construction with the latest
            // durable record while holding the same active -> thread lock order as
            // updates. A concurrent workspace/model/session/policy change makes
            // this engine stale; discard it and rebuild from the new snapshot.
            let thread_mutation = self.store.thread_mutation.lock();
            let record_is_current = self.store.load_thread(&thread.id)? == thread;
            if !record_is_current {
                drop(thread_mutation);
                drop(active);
                engine.cancel_with_reason(crate::core::engine::CancelReason::Internal);
                let _ = engine.try_send(Op::Shutdown);
                continue;
            }

            let evicted = enforce_lru_capacity(&mut active, self.manager_cfg.max_active_threads);
            active.engines.insert(
                thread.id.clone(),
                ActiveThreadState {
                    engine: engine.clone(),
                    active_turn: None,
                    route_identity,
                    route_model,
                    client_preflight_required: true,
                },
            );
            touch_lru(&mut active.lru, &thread.id);
            drop(thread_mutation);
            drop(active);
            for handle in evicted {
                let _ = handle.send(Op::Shutdown).await;
            }
            return Ok(engine);
        }
    }

    /// Get the engine handle for a thread, loading it if necessary.
    /// Public wrapper around the private `ensure_engine_loaded`.
    pub async fn get_engine(&self, thread_id: &str) -> Result<EngineHandle> {
        let thread = self.get_thread(thread_id).await?;
        self.ensure_engine_loaded(&thread).await
    }

    fn reconstruct_messages_from_turns(&self, turns: &[TurnRecord]) -> Result<Vec<Message>> {
        let mut messages = Vec::new();
        for turn in turns {
            let stored_items = self.store.list_items_for_turn(&turn.id)?;
            let items = if turn.item_ids.is_empty() {
                stored_items
            } else {
                let mut by_id: HashMap<String, TurnItemRecord> = stored_items
                    .iter()
                    .cloned()
                    .map(|item| (item.id.clone(), item))
                    .collect();
                let mut ordered = Vec::new();
                for item_id in &turn.item_ids {
                    if let Some(item) = by_id.remove(item_id) {
                        ordered.push(item);
                    }
                }
                for item in stored_items {
                    if by_id.contains_key(&item.id) {
                        ordered.push(item);
                    }
                }
                ordered
            };

            let mut assistant_blocks: Vec<ContentBlock> = Vec::new();
            let mut user_blocks: Vec<ContentBlock> = Vec::new();
            let flush_assistant = |blocks: &mut Vec<ContentBlock>, msgs: &mut Vec<Message>| {
                if !blocks.is_empty() {
                    msgs.push(Message {
                        role: "assistant".to_string(),
                        content: std::mem::take(blocks),
                    });
                }
            };
            let flush_user = |blocks: &mut Vec<ContentBlock>, msgs: &mut Vec<Message>| {
                if !blocks.is_empty() {
                    msgs.push(Message {
                        role: "user".to_string(),
                        content: std::mem::take(blocks),
                    });
                }
            };
            for item in items {
                match item.kind {
                    TurnItemKind::UserMessage => {
                        flush_assistant(&mut assistant_blocks, &mut messages);
                        let text = item.detail.unwrap_or(item.summary);
                        if !text.trim().is_empty() {
                            user_blocks.push(ContentBlock::Text {
                                text,
                                cache_control: None,
                            });
                        }
                    }
                    TurnItemKind::AgentMessage => {
                        flush_user(&mut user_blocks, &mut messages);
                        let text = item.detail.unwrap_or(item.summary);
                        if !text.trim().is_empty() {
                            assistant_blocks.push(ContentBlock::Text {
                                text,
                                cache_control: None,
                            });
                        }
                    }
                    TurnItemKind::AgentReasoning => {
                        flush_user(&mut user_blocks, &mut messages);
                        let thinking = item.detail.unwrap_or(item.summary);
                        if !thinking.trim().is_empty() {
                            assistant_blocks.push(ContentBlock::Thinking {
                                thinking,
                                signature: None,
                            });
                        }
                    }
                    TurnItemKind::ToolCall => {
                        let meta = item.metadata.as_ref();
                        let is_tool_result = meta.and_then(|m| m.get("tool_result_for")).is_some();
                        if is_tool_result {
                            flush_assistant(&mut assistant_blocks, &mut messages);
                            let tool_use_id = meta
                                .and_then(|m| m.get("tool_result_for"))
                                .and_then(|v| v.as_str())
                                .unwrap_or("")
                                .to_string();
                            let content = item.detail.unwrap_or_default();
                            let is_error = meta
                                .and_then(|m| m.get("is_error"))
                                .and_then(|v| v.as_bool())
                                .unwrap_or(false);
                            let content_blocks = meta
                                .and_then(|m| m.get("content_blocks"))
                                .and_then(|v| v.as_array())
                                .cloned();
                            user_blocks.push(ContentBlock::ToolResult {
                                tool_use_id,
                                content,
                                is_error: if is_error { Some(true) } else { None },
                                content_blocks,
                            });
                        } else {
                            flush_user(&mut user_blocks, &mut messages);
                            let tool_use_id = meta
                                .and_then(|m| m.get("tool_use_id"))
                                .and_then(|v| v.as_str())
                                .unwrap_or("")
                                .to_string();
                            let tool_name = meta
                                .and_then(|m| m.get("tool_name"))
                                .and_then(|v| v.as_str())
                                .unwrap_or("")
                                .to_string();
                            let input_str = item.detail.unwrap_or_default();
                            let input: serde_json::Value =
                                serde_json::from_str(&input_str).unwrap_or(serde_json::Value::Null);
                            assistant_blocks.push(ContentBlock::ToolUse {
                                id: tool_use_id,
                                name: tool_name,
                                input,
                                caller: None,
                            });
                        }
                    }
                    _ => {}
                }
            }
            flush_assistant(&mut assistant_blocks, &mut messages);
            flush_user(&mut user_blocks, &mut messages);
        }
        Ok(messages)
    }

    async fn monitor_turn(
        &self,
        thread_id: String,
        turn_id: String,
        engine: EngineHandle,
    ) -> Result<()> {
        let mut current_message_item: Option<(String, String)> = None;
        let mut current_reasoning_item: Option<(String, String)> = None;
        let mut tool_items: HashMap<String, String> = HashMap::new();
        let mut compaction_items: HashMap<String, String> = HashMap::new();
        let mut turn_usage: Option<Usage> = None;
        let mut turn_base_url: Option<String> = None;
        let mut turn_status: Option<RuntimeTurnStatus> = None;
        let mut turn_error: Option<String> = None;
        let mut saw_engine_activity = false;
        let mut saw_turn_started = false;

        loop {
            let event = {
                let mut rx = engine.rx_event.write().await;
                rx.recv().await
            };
            let Some(event) = event else {
                if self
                    .is_interrupt_requested(&thread_id, &turn_id)
                    .await
                    .unwrap_or(false)
                {
                    turn_status = Some(RuntimeTurnStatus::Interrupted);
                    break;
                }
                bail!("engine event channel closed before turn {turn_id} completed");
            };

            // SyncSession and configuration operations emit control status
            // receipts on the same channel before SendMessage is processed.
            // They belong to engine setup, not to the next claimed turn.
            if !saw_turn_started
                && matches!(
                    &event,
                    EngineEvent::Status { .. }
                        | EngineEvent::SessionUpdated { .. }
                        | EngineEvent::AgentList { .. }
                        | EngineEvent::AgentSpawned { .. }
                        | EngineEvent::AgentProgress { .. }
                        | EngineEvent::AgentComplete { .. }
                )
            {
                continue;
            }

            // Engine configuration and session synchronization can emit
            // Status/SessionUpdated events before a turn is claimed. Those
            // control-plane receipts share the engine channel, but they are
            // not model output and must not make an otherwise empty turn look
            // successful. Count only events that carry turn-scoped work or
            // user-visible output.
            if matches!(
                &event,
                EngineEvent::MessageStarted { .. }
                    | EngineEvent::MessageDelta { .. }
                    | EngineEvent::MessageComplete { .. }
                    | EngineEvent::ThinkingStarted { .. }
                    | EngineEvent::ThinkingDelta { .. }
                    | EngineEvent::ThinkingComplete { .. }
                    | EngineEvent::ToolCallStarted { .. }
                    | EngineEvent::ToolCallComplete { .. }
                    | EngineEvent::CompactionStarted { .. }
                    | EngineEvent::CompactionCompleted { .. }
                    | EngineEvent::CompactionFailed { .. }
                    | EngineEvent::AgentSpawned { .. }
                    | EngineEvent::AgentProgress { .. }
                    | EngineEvent::AgentComplete { .. }
                    | EngineEvent::ApprovalRequired { .. }
                    | EngineEvent::ElevationRequired { .. }
                    | EngineEvent::UserInputRequired { .. }
                    | EngineEvent::Error { .. }
            ) {
                saw_engine_activity = true;
            }

            match event {
                EngineEvent::TurnStarted { .. } => {
                    saw_turn_started = true;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        None,
                        "turn.lifecycle",
                        json!({ "status": "in_progress" }),
                    )
                    .await?;
                }
                EngineEvent::MessageStarted { .. } => {
                    let item_id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: item_id.clone(),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::AgentMessage,
                        status: TurnItemLifecycleStatus::InProgress,
                        summary: String::new(),
                        detail: Some(String::new()),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: None,
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item_id),
                        "item.started",
                        json!({ "item": item }),
                    )
                    .await?;
                    current_message_item = Some((item_id, String::new()));
                }
                EngineEvent::MessageDelta { content, .. } => {
                    if let Some((item_id, text)) = current_message_item.as_mut() {
                        text.push_str(&content);
                        self.emit_event(
                            &thread_id,
                            Some(&turn_id),
                            Some(item_id),
                            "item.delta",
                            json!({ "delta": content, "kind": "agent_message" }),
                        )
                        .await?;
                    }
                }
                EngineEvent::MessageComplete { .. } => {
                    if let Some((item_id, text)) = current_message_item.take() {
                        let mut item = self.store.load_item(&item_id)?;
                        item.status = TurnItemLifecycleStatus::Completed;
                        item.summary = summarize_text(&text, SUMMARY_LIMIT);
                        item.detail = Some(text);
                        item.ended_at = Some(Utc::now());
                        self.store.save_item(&item)?;
                        self.emit_event(
                            &thread_id,
                            Some(&turn_id),
                            Some(&item_id),
                            "item.completed",
                            json!({ "item": item }),
                        )
                        .await?;
                    }
                }
                EngineEvent::ThinkingStarted { .. } => {
                    let item_id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: item_id.clone(),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::AgentReasoning,
                        status: TurnItemLifecycleStatus::InProgress,
                        summary: String::new(),
                        detail: Some(String::new()),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: None,
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item_id),
                        "item.started",
                        json!({ "item": item }),
                    )
                    .await?;
                    current_reasoning_item = Some((item_id, String::new()));
                }
                EngineEvent::ThinkingDelta { content, .. } => {
                    if let Some((item_id, text)) = current_reasoning_item.as_mut() {
                        text.push_str(&content);
                        self.emit_event(
                            &thread_id,
                            Some(&turn_id),
                            Some(item_id),
                            "item.delta",
                            json!({ "delta": content, "kind": "agent_reasoning" }),
                        )
                        .await?;
                    }
                }
                EngineEvent::ThinkingComplete { .. } => {
                    if let Some((item_id, text)) = current_reasoning_item.take() {
                        let mut item = self.store.load_item(&item_id)?;
                        item.status = TurnItemLifecycleStatus::Completed;
                        item.summary = summarize_text(&text, SUMMARY_LIMIT);
                        item.detail = Some(text);
                        item.ended_at = Some(Utc::now());
                        self.store.save_item(&item)?;
                        self.emit_event(
                            &thread_id,
                            Some(&turn_id),
                            Some(&item_id),
                            "item.completed",
                            json!({ "item": item }),
                        )
                        .await?;
                    }
                }
                EngineEvent::ToolCallStarted { id, name, input } => {
                    let item_id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
                    tool_items.insert(id.clone(), item_id.clone());
                    let kind = tool_kind_for_name(&name);
                    let summary = summarize_text(&format!("{name} started"), SUMMARY_LIMIT);
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: item_id.clone(),
                        turn_id: turn_id.clone(),
                        kind,
                        status: TurnItemLifecycleStatus::InProgress,
                        summary,
                        detail: Some(serde_json::to_string(&input).unwrap_or_default()),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: None,
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item_id),
                        "item.started",
                        json!({ "item": item, "tool": { "id": id, "name": name, "input": input } }),
                    )
                    .await?;
                }
                EngineEvent::ToolCallComplete { id, name, result } => {
                    if let Some(item_id) = tool_items.remove(&id) {
                        let mut item = self.store.load_item(&item_id)?;
                        let now = Utc::now();
                        item.ended_at = Some(now);
                        match result {
                            Ok(output) => {
                                item.status = if output.success {
                                    TurnItemLifecycleStatus::Completed
                                } else {
                                    TurnItemLifecycleStatus::Failed
                                };
                                item.summary = summarize_text(
                                    &format!("{name}: {}", output.content),
                                    SUMMARY_LIMIT,
                                );
                                item.detail = Some(output.content.clone());
                                item.metadata = output.metadata.clone();
                            }
                            Err(err) => {
                                item.status = TurnItemLifecycleStatus::Failed;
                                item.summary =
                                    summarize_text(&format!("{name} failed: {err}"), SUMMARY_LIMIT);
                                item.detail = Some(err.to_string());
                            }
                        }
                        self.store.save_item(&item)?;
                        self.emit_event(
                            &thread_id,
                            Some(&turn_id),
                            Some(&item_id),
                            if item.status == TurnItemLifecycleStatus::Completed {
                                "item.completed"
                            } else {
                                "item.failed"
                            },
                            json!({ "item": item }),
                        )
                        .await?;
                    }
                }
                EngineEvent::CompactionStarted { id, auto, message } => {
                    let item_id = format!("item_{}", &Uuid::new_v4().to_string()[..8]);
                    compaction_items.insert(id.clone(), item_id.clone());
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: item_id.clone(),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::ContextCompaction,
                        status: TurnItemLifecycleStatus::InProgress,
                        summary: summarize_text(&message, SUMMARY_LIMIT),
                        detail: Some(message.clone()),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: None,
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item_id),
                        "item.started",
                        json!({ "item": item, "auto": auto }),
                    )
                    .await?;
                }
                EngineEvent::CompactionCompleted {
                    id,
                    auto,
                    message,
                    messages_before,
                    messages_after,
                    summary_prompt,
                } => {
                    // Persist the summary into the thread record so engine
                    // reloads (LRU eviction / restart) restore it: reload
                    // passes the record prompt through SyncSession, where
                    // `extract_compaction_summary_prompt` picks the summary
                    // back up. Without this the summary lives only in engine
                    // memory and silently dies with the engine.
                    if let Some(summary) =
                        summary_prompt.as_deref().filter(|s| !s.trim().is_empty())
                    {
                        let persist_summary = (|| -> Result<()> {
                            let _thread_mutation = self.store.thread_mutation.lock();
                            let mut thread = self.store.load_thread(&thread_id)?;
                            let merged =
                                merge_summary_into_prompt(thread.system_prompt.as_deref(), summary);
                            if thread.system_prompt.as_deref() != Some(merged.as_str()) {
                                thread.system_prompt = Some(merged);
                                thread.updated_at = Utc::now();
                                self.store.save_thread(&thread)?;
                            }
                            Ok(())
                        })();
                        if let Err(e) = persist_summary {
                            tracing::warn!(
                                thread_id = %thread_id,
                                "Failed to persist compaction summary to thread record: {e}"
                            );
                        }
                    }
                    if let Some(item_id) = compaction_items.remove(&id) {
                        let mut item = self.store.load_item(&item_id)?;
                        item.status = TurnItemLifecycleStatus::Completed;
                        item.summary = summarize_text(&message, SUMMARY_LIMIT);
                        item.detail = Some(message);
                        item.ended_at = Some(Utc::now());
                        self.store.save_item(&item)?;
                        self.emit_event(
                            &thread_id,
                            Some(&turn_id),
                            Some(&item_id),
                            "item.completed",
                            json!({
                                "item": item,
                                "auto": auto,
                                "messages_before": messages_before,
                                "messages_after": messages_after,
                            }),
                        )
                        .await?;
                    }
                }
                EngineEvent::CompactionFailed { id, auto, message } => {
                    if let Some(item_id) = compaction_items.remove(&id) {
                        let mut item = self.store.load_item(&item_id)?;
                        item.status = TurnItemLifecycleStatus::Failed;
                        item.summary = summarize_text(&message, SUMMARY_LIMIT);
                        item.detail = Some(message);
                        item.ended_at = Some(Utc::now());
                        self.store.save_item(&item)?;
                        self.emit_event(
                            &thread_id,
                            Some(&turn_id),
                            Some(&item_id),
                            "item.failed",
                            json!({ "item": item, "auto": auto }),
                        )
                        .await?;
                    }
                }
                EngineEvent::AgentSpawned { id, prompt, .. } => {
                    let message = format!(
                        "Sub-agent {id} spawned: {}",
                        summarize_text(&prompt, SUMMARY_LIMIT)
                    );
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: format!("item_{}", &Uuid::new_v4().to_string()[..8]),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::Status,
                        status: TurnItemLifecycleStatus::Completed,
                        summary: summarize_text(&message, SUMMARY_LIMIT),
                        detail: Some(message),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: Some(Utc::now()),
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item.id),
                        "agent.spawned",
                        json!({ "item": item, "agent_id": id }),
                    )
                    .await?;
                }
                EngineEvent::AgentProgress { id, status, .. } => {
                    let message = format!("Sub-agent {id}: {status}");
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: format!("item_{}", &Uuid::new_v4().to_string()[..8]),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::Status,
                        status: TurnItemLifecycleStatus::Completed,
                        summary: summarize_text(&message, SUMMARY_LIMIT),
                        detail: Some(message),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: Some(Utc::now()),
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item.id),
                        "agent.progress",
                        json!({ "item": item, "agent_id": id }),
                    )
                    .await?;
                }
                EngineEvent::AgentComplete { id, result, .. } => {
                    let message = format!(
                        "Sub-agent {id} completed: {}",
                        summarize_text(&result, SUMMARY_LIMIT)
                    );
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: format!("item_{}", &Uuid::new_v4().to_string()[..8]),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::Status,
                        status: TurnItemLifecycleStatus::Completed,
                        summary: summarize_text(&message, SUMMARY_LIMIT),
                        detail: Some(message),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: Some(Utc::now()),
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item.id),
                        "agent.completed",
                        json!({ "item": item, "agent_id": id }),
                    )
                    .await?;
                }
                EngineEvent::AgentList { agents } => {
                    let running = agents
                        .iter()
                        .filter(|agent| matches!(agent.status, SubAgentStatus::Running))
                        .count();
                    let interrupted = agents
                        .iter()
                        .filter(|agent| matches!(agent.status, SubAgentStatus::Interrupted(_)))
                        .count();
                    let completed = agents
                        .iter()
                        .filter(|agent| matches!(agent.status, SubAgentStatus::Completed))
                        .count();
                    let message = format!(
                        "Sub-agent list refreshed: {} total ({running} running, {interrupted} interrupted, {completed} completed)",
                        agents.len()
                    );
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: format!("item_{}", &Uuid::new_v4().to_string()[..8]),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::Status,
                        status: TurnItemLifecycleStatus::Completed,
                        summary: summarize_text(&message, SUMMARY_LIMIT),
                        detail: Some(message),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: Some(Utc::now()),
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item.id),
                        "agent.list",
                        json!({ "item": item, "agents": agents }),
                    )
                    .await?;
                }
                EngineEvent::ApprovalRequired {
                    id,
                    tool_name,
                    description,
                    intent_summary,
                    ..
                } => {
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        None,
                        "approval.required",
                        json!({
                            "id": id,
                            "approval_id": id,
                            "tool_name": tool_name,
                            "description": description,
                            "intent_summary": intent_summary,
                        }),
                    )
                    .await?;

                    let Some((auto_approve, trust_mode)) =
                        self.active_turn_flags(&thread_id, &turn_id).await
                    else {
                        let _ = engine.deny_tool_call(id).await;
                        continue;
                    };

                    if auto_approve {
                        let auto_decision =
                            Self::approval_decision(auto_approve, trust_mode, false);
                        let (dec_str, approved) = match auto_decision {
                            RuntimeApprovalDecision::ApproveTool => ("allow", true),
                            RuntimeApprovalDecision::DenyTool
                            | RuntimeApprovalDecision::RetryWithFullAccess => ("deny", false),
                        };
                        // Emit approval.decided so external clients (GUI)
                        // know the approval was resolved automatically and
                        // can clear any pending approval UI.  Without this
                        // event the GUI would show a frozen approval dialog
                        // that never receives approval.decided.
                        self.emit_event(
                            &thread_id,
                            Some(&turn_id),
                            None,
                            "approval.decided",
                            json!({
                                "approval_id": id,
                                "decision": dec_str,
                                "remember": false,
                                "auto": true,
                            }),
                        )
                        .await
                        .ok();
                        if approved {
                            let _ = engine.approve_tool_call(id).await;
                        } else {
                            let _ = engine.deny_tool_call(id).await;
                        }
                        continue;
                    }

                    let rx = self.register_pending_approval(&id);
                    let approval_timeout = approval_decision_timeout();
                    match tokio::time::timeout(approval_timeout, rx).await {
                        Ok(Ok(ExternalApprovalDecision::Allow { remember })) => {
                            if remember {
                                self.remember_thread_auto_approve(&thread_id).await;
                            }
                            self.emit_event(
                                &thread_id,
                                Some(&turn_id),
                                None,
                                "approval.decided",
                                json!({
                                    "approval_id": id,
                                    "decision": "allow",
                                    "remember": remember,
                                }),
                            )
                            .await
                            .ok();
                            let _ = engine.approve_tool_call(id).await;
                        }
                        Ok(Ok(ExternalApprovalDecision::Deny { remember })) => {
                            self.emit_event(
                                &thread_id,
                                Some(&turn_id),
                                None,
                                "approval.decided",
                                json!({
                                    "approval_id": id,
                                    "decision": "deny",
                                    "remember": remember,
                                }),
                            )
                            .await
                            .ok();
                            let _ = engine.deny_tool_call(id).await;
                        }
                        Ok(Err(_recv_err)) => {
                            self.cancel_pending_approval(&id);
                            let _ = engine.deny_tool_call(id).await;
                        }
                        Err(_timeout) => {
                            self.cancel_pending_approval(&id);
                            self.emit_event(
                                &thread_id,
                                Some(&turn_id),
                                None,
                                "approval.timeout",
                                json!({
                                    "approval_id": id,
                                    "timeout_secs": approval_timeout.as_secs(),
                                }),
                            )
                            .await
                            .ok();
                            self.emit_event(
                                &thread_id,
                                Some(&turn_id),
                                None,
                                "approval.decided",
                                json!({
                                    "approval_id": id,
                                    "decision": "deny",
                                    "remember": false,
                                    "timeout": true,
                                }),
                            )
                            .await
                            .ok();
                            let _ = engine.deny_tool_call(id).await;
                        }
                    }
                }
                EngineEvent::ElevationRequired {
                    tool_id,
                    tool_name,
                    denial_reason,
                    ..
                } => {
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        None,
                        "sandbox.denied",
                        json!({
                            "tool_id": tool_id,
                            "tool_name": tool_name,
                            "reason": denial_reason,
                        }),
                    )
                    .await?;
                    let (auto_approve, trust_mode) = self
                        .active_turn_flags(&thread_id, &turn_id)
                        .await
                        .unwrap_or((false, false));
                    match Self::approval_decision(auto_approve, trust_mode, true) {
                        RuntimeApprovalDecision::RetryWithFullAccess => {
                            let _ = engine
                                .retry_tool_with_policy(
                                    tool_id,
                                    crate::sandbox::SandboxPolicy::DangerFullAccess,
                                )
                                .await;
                        }
                        RuntimeApprovalDecision::ApproveTool
                        | RuntimeApprovalDecision::DenyTool => {
                            let _ = engine.deny_tool_call(tool_id).await;
                        }
                    }
                }
                EngineEvent::UserInputRequired { id, request } => {
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        None,
                        "user_input.required",
                        json!({
                            "id": id,
                            "request": request,
                        }),
                    )
                    .await?;
                }
                EngineEvent::Status { message } => {
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: format!("item_{}", &Uuid::new_v4().to_string()[..8]),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::Status,
                        status: TurnItemLifecycleStatus::Completed,
                        summary: summarize_text(&message, SUMMARY_LIMIT),
                        detail: Some(message.clone()),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: Some(Utc::now()),
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item.id),
                        "item.completed",
                        json!({ "item": item }),
                    )
                    .await?;
                }
                EngineEvent::Error { envelope, .. } => {
                    turn_status = Some(RuntimeTurnStatus::Failed);
                    turn_error = Some(envelope.message.clone());
                    let message = envelope.message.clone();
                    let item = TurnItemRecord {
                        schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                        id: format!("item_{}", &Uuid::new_v4().to_string()[..8]),
                        turn_id: turn_id.clone(),
                        kind: TurnItemKind::Error,
                        status: TurnItemLifecycleStatus::Failed,
                        summary: summarize_text(&message, SUMMARY_LIMIT),
                        detail: Some(message),
                        metadata: None,
                        artifact_refs: Vec::new(),
                        started_at: Some(Utc::now()),
                        ended_at: Some(Utc::now()),
                    };
                    self.store.save_item(&item)?;
                    self.attach_item_to_turn(&turn_id, &item.id)?;
                    self.emit_event(
                        &thread_id,
                        Some(&turn_id),
                        Some(&item.id),
                        "item.failed",
                        json!({ "item": item }),
                    )
                    .await?;
                }
                EngineEvent::TurnComplete {
                    usage,
                    status,
                    error,
                    base_url,
                    ..
                } => {
                    turn_usage = Some(usage);
                    turn_base_url = base_url;
                    let reported_status = match status {
                        TurnOutcomeStatus::Completed => RuntimeTurnStatus::Completed,
                        TurnOutcomeStatus::Interrupted => RuntimeTurnStatus::Interrupted,
                        TurnOutcomeStatus::Failed => RuntimeTurnStatus::Failed,
                    };
                    // Some engines emit a categorized Error followed by their
                    // generic TurnComplete(Completed) cleanup receipt. Keep
                    // the error authoritative instead of silently converting
                    // a failed turn back to success.
                    turn_status = Some(
                        if turn_status == Some(RuntimeTurnStatus::Failed)
                            && reported_status == RuntimeTurnStatus::Completed
                        {
                            RuntimeTurnStatus::Failed
                        } else {
                            reported_status
                        },
                    );
                    if let Some(err) = error {
                        turn_error = Some(err);
                    }
                    break;
                }
                _ => {}
            }
        }

        let mut turn_status = turn_status
            .expect("turn monitor exits normally only after assigning a terminal status");

        if self
            .is_interrupt_requested(&thread_id, &turn_id)
            .await
            .unwrap_or(false)
        {
            turn_status = RuntimeTurnStatus::Interrupted;
        }

        if let Some((item_id, text)) = current_message_item.take() {
            let mut item = self.store.load_item(&item_id)?;
            if turn_status == RuntimeTurnStatus::Interrupted {
                item.status = TurnItemLifecycleStatus::Interrupted;
            } else {
                item.status = TurnItemLifecycleStatus::Completed;
            }
            item.summary = summarize_text(&text, SUMMARY_LIMIT);
            item.detail = Some(text);
            item.ended_at = Some(Utc::now());
            self.store.save_item(&item)?;
            self.emit_event(
                &thread_id,
                Some(&turn_id),
                Some(&item_id),
                if item.status == TurnItemLifecycleStatus::Interrupted {
                    "item.interrupted"
                } else {
                    "item.completed"
                },
                json!({ "item": item }),
            )
            .await?;
        }

        if let Some((item_id, text)) = current_reasoning_item.take() {
            let mut item = self.store.load_item(&item_id)?;
            if turn_status == RuntimeTurnStatus::Interrupted {
                item.status = TurnItemLifecycleStatus::Interrupted;
            } else {
                item.status = TurnItemLifecycleStatus::Completed;
            }
            item.summary = summarize_text(&text, SUMMARY_LIMIT);
            item.detail = Some(text);
            item.ended_at = Some(Utc::now());
            self.store.save_item(&item)?;
            self.emit_event(
                &thread_id,
                Some(&turn_id),
                Some(&item_id),
                if item.status == TurnItemLifecycleStatus::Interrupted {
                    "item.interrupted"
                } else {
                    "item.completed"
                },
                json!({ "item": item }),
            )
            .await?;
        }

        if turn_status == RuntimeTurnStatus::Completed && !saw_engine_activity {
            turn_status = RuntimeTurnStatus::Failed;
            turn_error = Some(EMPTY_TURN_REASON.to_string());
            let item = TurnItemRecord {
                schema_version: CURRENT_RUNTIME_SCHEMA_VERSION,
                id: format!("item_{}", &Uuid::new_v4().to_string()[..8]),
                turn_id: turn_id.clone(),
                kind: TurnItemKind::Error,
                status: TurnItemLifecycleStatus::Failed,
                summary: EMPTY_TURN_REASON.to_string(),
                detail: Some(EMPTY_TURN_REASON.to_string()),
                metadata: None,
                artifact_refs: Vec::new(),
                started_at: Some(Utc::now()),
                ended_at: Some(Utc::now()),
            };
            self.store.save_item(&item)?;
            self.attach_item_to_turn(&turn_id, &item.id)?;
            self.emit_event(
                &thread_id,
                Some(&turn_id),
                Some(&item.id),
                "item.failed",
                json!({ "item": item }),
            )
            .await?;
        }

        let ended_at = Utc::now();
        let turn = {
            let _turn_mutation = self.store.turn_mutation.lock();
            let mut turn = self.store.load_turn(&turn_id)?;
            turn.status = turn_status;
            turn.ended_at = Some(ended_at);
            turn.duration_ms = turn.started_at.map(|start| duration_ms(start, ended_at));
            turn.usage = turn_usage;
            turn.effective_billing_surface = turn
                .effective_provider
                .as_deref()
                .and_then(ApiProvider::parse)
                .and_then(|provider| {
                    crate::pricing::billing_surface_for_route(provider, turn_base_url.as_deref())
                })
                .map(str::to_string);
            turn.error = turn_error;
            self.store.save_turn(&turn)?;
            turn
        };

        {
            let _thread_mutation = self.store.thread_mutation.lock();
            let mut thread = self.store.load_thread(&thread_id)?;
            thread.latest_turn_id = Some(turn_id.clone());
            thread.updated_at = Utc::now();
            self.store.save_thread(&thread)?;
        }

        self.emit_event(
            &thread_id,
            Some(&turn_id),
            None,
            "turn.completed",
            json!({ "turn": turn.clone() }),
        )
        .await?;

        {
            let mut active = self.active.lock().await;
            if let Some(state) = active.engines.get_mut(&thread_id)
                && state
                    .active_turn
                    .as_ref()
                    .is_some_and(|t| t.turn_id == turn_id)
            {
                state.active_turn = None;
            }
            touch_lru(&mut active.lru, &thread_id);
        }

        Ok(())
    }

    fn attach_item_to_turn(&self, turn_id: &str, item_id: &str) -> Result<()> {
        let _turn_mutation = self.store.turn_mutation.lock();
        let mut turn = self.store.load_turn(turn_id)?;
        if !turn.item_ids.iter().any(|id| id == item_id) {
            turn.item_ids.push(item_id.to_string());
            self.store.save_turn(&turn)?;
        }
        Ok(())
    }

    async fn is_interrupt_requested(&self, thread_id: &str, turn_id: &str) -> Result<bool> {
        let active = self.active.lock().await;
        let Some(state) = active.engines.get(thread_id) else {
            return Ok(false);
        };
        let Some(turn) = state.active_turn.as_ref() else {
            return Ok(false);
        };
        Ok(turn.turn_id == turn_id && turn.interrupt_requested)
    }

    async fn active_turn_flags(&self, thread_id: &str, turn_id: &str) -> Option<(bool, bool)> {
        let active = self.active.lock().await;
        let state = active.engines.get(thread_id)?;
        let turn = state.active_turn.as_ref()?;
        if turn.turn_id != turn_id {
            return None;
        }
        Some((turn.auto_approve, turn.trust_mode))
    }

    async fn active_turn_id(&self, thread_id: &str) -> Option<String> {
        let active = self.active.lock().await;
        active
            .engines
            .get(thread_id)?
            .active_turn
            .as_ref()
            .map(|turn| turn.turn_id.clone())
    }

    fn approval_decision(
        auto_approve: bool,
        trust_mode: bool,
        requires_full_access: bool,
    ) -> RuntimeApprovalDecision {
        if !auto_approve {
            return RuntimeApprovalDecision::DenyTool;
        }
        if requires_full_access {
            if trust_mode {
                RuntimeApprovalDecision::RetryWithFullAccess
            } else {
                RuntimeApprovalDecision::DenyTool
            }
        } else {
            RuntimeApprovalDecision::ApproveTool
        }
    }

    fn recover_interrupted_state(&self) -> Result<()> {
        let now = Utc::now();
        // One scan of the turns store, filtered to the interrupted-candidate
        // statuses, grouped by thread. The previous shape re-read and
        // re-parsed every turn file once per thread — O(threads x turns) on
        // the boot path (#3757).
        let mut turns_by_thread: HashMap<String, Vec<TurnRecord>> = HashMap::new();
        for turn in self.store.list_all_turns()? {
            if matches!(
                turn.status,
                RuntimeTurnStatus::Queued | RuntimeTurnStatus::InProgress
            ) {
                turns_by_thread
                    .entry(turn.thread_id.clone())
                    .or_default()
                    .push(turn);
            }
        }
        if turns_by_thread.is_empty() {
            return Ok(());
        }
        for mut thread in self.store.list_threads()? {
            let Some(turns) = turns_by_thread.remove(&thread.id) else {
                continue;
            };
            let mut thread_changed = false;
            for mut turn in turns {
                turn.status = RuntimeTurnStatus::Interrupted;
                turn.error = Some(RUNTIME_RESTART_REASON.to_string());
                turn.ended_at = Some(now);
                if let Some(started_at) = turn.started_at {
                    let elapsed = now.signed_duration_since(started_at);
                    turn.duration_ms = Some(elapsed.num_milliseconds().max(0) as u64);
                }
                self.store.save_turn(&turn)?;

                for item_id in &turn.item_ids {
                    let mut item = self.store.load_item(item_id)?;
                    if matches!(
                        item.status,
                        TurnItemLifecycleStatus::Queued | TurnItemLifecycleStatus::InProgress
                    ) {
                        item.status = TurnItemLifecycleStatus::Interrupted;
                        item.ended_at = Some(now);
                        self.store.save_item(&item)?;
                    }
                }

                thread.updated_at = now;
                thread_changed = true;
            }

            if thread_changed {
                self.store.save_thread(&thread)?;
            }
        }

        Ok(())
    }

    #[cfg(test)]
    pub(crate) async fn install_test_engine(
        &self,
        thread_id: &str,
        engine: EngineHandle,
    ) -> Result<()> {
        let thread = self.get_thread(thread_id).await?;
        let config = self.read_config().clone();
        let route = self.resolved_route_for_thread(&config, &thread)?;
        let mut active = self.active.lock().await;
        active.engines.insert(
            thread_id.to_string(),
            ActiveThreadState {
                engine,
                active_turn: None,
                route_identity: route.identity,
                route_model: route.model,
                client_preflight_required: false,
            },
        );
        touch_lru(&mut active.lru, thread_id);
        Ok(())
    }
}

fn dynamic_tool_result_text(content: &[DynamicToolCallContent]) -> String {
    content
        .iter()
        .map(|item| match item {
            DynamicToolCallContent::InputText { text } => text.clone(),
            DynamicToolCallContent::InputImage { image_url } => format!("[image] {image_url}"),
        })
        .collect::<Vec<_>>()
        .join("\n")
}

#[async_trait::async_trait]
impl crate::tools::spec::DynamicToolExecutor for RuntimeThreadManager {
    async fn execute_dynamic_tool(
        &self,
        thread_id: Option<String>,
        namespace: Option<String>,
        name: String,
        input: Value,
    ) -> std::result::Result<crate::tools::spec::ToolResult, crate::tools::spec::ToolError> {
        let thread_id = thread_id.ok_or_else(|| {
            crate::tools::spec::ToolError::not_available(format!(
                "runtime dynamic tool '{name}' has no active thread"
            ))
        })?;
        let turn_id = self.active_turn_id(&thread_id).await.ok_or_else(|| {
            crate::tools::spec::ToolError::not_available(format!(
                "runtime dynamic tool '{name}' has no active turn"
            ))
        })?;
        let call_id = format!("call_{}", &Uuid::new_v4().to_string()[..8]);
        let rx = self.register_pending_dynamic_tool(&call_id);

        let params = DynamicToolCallParams {
            thread_id: thread_id.clone(),
            turn_id: turn_id.clone(),
            call_id: call_id.clone(),
            namespace,
            tool: name.clone(),
            arguments: input,
        };
        if let Err(err) = self
            .emit_event(
                &thread_id,
                Some(&turn_id),
                None,
                "tool_call.requested",
                json!(params),
            )
            .await
        {
            self.cancel_pending_dynamic_tool(&call_id);
            return Err(crate::tools::spec::ToolError::execution_failed(format!(
                "failed to emit runtime dynamic tool request for '{name}': {err}"
            )));
        }

        let approval_timeout = approval_decision_timeout();
        match tokio::time::timeout(approval_timeout, rx).await {
            Ok(Ok(result)) => {
                let text = dynamic_tool_result_text(&result.content);
                if result.success {
                    Ok(crate::tools::spec::ToolResult::success(text))
                } else {
                    Ok(crate::tools::spec::ToolResult::error(if text.is_empty() {
                        "dynamic tool failed".to_string()
                    } else {
                        text
                    }))
                }
            }
            Ok(Err(_recv_err)) => Err(crate::tools::spec::ToolError::execution_failed(format!(
                "runtime dynamic tool '{name}' result channel closed"
            ))),
            Err(_timeout) => {
                self.cancel_pending_dynamic_tool(&call_id);
                Err(crate::tools::spec::ToolError::Timeout {
                    seconds: approval_timeout.as_secs(),
                })
            }
        }
    }
}

fn touch_lru(lru: &mut VecDeque<String>, thread_id: &str) {
    if let Some(idx) = lru.iter().position(|id| id == thread_id) {
        lru.remove(idx);
    }
    lru.push_back(thread_id.to_string());
}

fn enforce_lru_capacity(
    active: &mut ActiveThreads,
    max_active_threads: usize,
) -> Vec<EngineHandle> {
    let mut evicted = Vec::new();
    if max_active_threads == 0 || active.engines.len() < max_active_threads {
        return evicted;
    }
    let protected = active
        .engines
        .iter()
        .filter_map(|(thread_id, state)| {
            if state.active_turn.is_some() {
                Some(thread_id.clone())
            } else {
                None
            }
        })
        .collect::<HashSet<_>>();

    let scan_limit = active.lru.len();
    for _ in 0..scan_limit {
        let Some(candidate) = active.lru.pop_front() else {
            break;
        };
        if protected.contains(&candidate) {
            active.lru.push_back(candidate);
            continue;
        }
        if let Some(state) = active.engines.remove(&candidate) {
            evicted.push(state.engine);
        }
        break;
    }
    evicted
}

/// Resolves only explicit mode tokens to an app mode. Free-form prompt text is
/// never a valid mode token: `parse_mode_opt` returns `None` unless the input is
/// exactly `agent`/`plan`/`yolo` or numeric aliases `1`/`2`/`4`. Mode
/// changes originate from the Tab cycle, `/mode`, the mode picker, or
/// config/startup defaults, not from submitted natural-language prompt text.
///
/// Textual `auto` is a legacy alias for Agent while Auto is deferred (#3733).
fn parse_mode_opt(mode: &str) -> Option<AppMode> {
    match mode.trim().to_ascii_lowercase().as_str() {
        "agent" | "auto" | "1" => Some(AppMode::Agent),
        "plan" | "2" => Some(AppMode::Plan),
        "yolo" | "4" | "bypass" | "bypass-permissions" | "bypasspermissions" => Some(AppMode::Yolo),
        _ => None,
    }
}

fn parse_mode(mode: &str) -> AppMode {
    parse_mode_opt(mode).unwrap_or(AppMode::Agent)
}

fn tool_kind_for_name(name: &str) -> TurnItemKind {
    let lower = name.to_ascii_lowercase();
    if lower == "exec_shell" || lower == "exec_shell_wait" || lower == "exec_shell_interact" {
        return TurnItemKind::CommandExecution;
    }
    if lower.contains("patch") || lower.contains("write") || lower.contains("edit") {
        return TurnItemKind::FileChange;
    }
    TurnItemKind::ToolCall
}

/// One sub-agent rebind hint extracted from a thread's persisted event
/// timeline (issue #128). When the TUI resumes a session that was
/// mid-fanout, the in-transcript card stack is empty — these hints let the
/// UI know which agent_ids were live (or recently terminal) so it can
/// reconstruct the matching `DelegateCard` / `FanoutCard` placeholders
/// before fresh mailbox envelopes arrive on a re-attached engine.
///
/// The helper is the testable contract here — actual TUI wire-up to the
/// resume flow is a follow-up; the runtime API consumer (`runtime_api.rs`)
/// can already call `resume_thread_with_agent_rebind` to drive it.
#[derive(Debug, Clone, PartialEq, Eq)]
#[allow(dead_code)] // consumed by #128 follow-up TUI resume wiring; tested here.
pub struct AgentRebindHint {
    pub agent_id: String,
    pub status: AgentRebindStatus,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[allow(dead_code)]
pub enum AgentRebindStatus {
    Spawned,
    InProgress,
    Completed,
}

/// Collapse a chronologically ordered slice of `RuntimeEventRecord` into
/// the latest known status per `agent_id`. Drops entries that aren't in
/// the `agent.*` family. Cards built from these hints are immediately
/// open to mutation by subsequent live mailbox envelopes (each envelope's
/// `agent_id` matches one already in the rebind map).
#[must_use]
#[allow(dead_code)]
pub fn collect_agent_rebind_hints(events: &[RuntimeEventRecord]) -> Vec<AgentRebindHint> {
    use std::collections::BTreeMap;
    let mut latest: BTreeMap<String, AgentRebindStatus> = BTreeMap::new();
    for event in events {
        let id = match event.payload.get("agent_id").and_then(|v| v.as_str()) {
            Some(id) => id.to_string(),
            None => continue,
        };
        let next_status = match event.event.as_str() {
            "agent.spawned" => Some(AgentRebindStatus::Spawned),
            "agent.progress" => Some(AgentRebindStatus::InProgress),
            "agent.completed" => Some(AgentRebindStatus::Completed),
            _ => None,
        };
        if let Some(status) = next_status {
            // Don't downgrade Completed → InProgress on out-of-order events.
            let entry = latest.entry(id).or_insert(status);
            if !matches!(*entry, AgentRebindStatus::Completed) {
                *entry = status;
            }
        }
    }
    latest
        .into_iter()
        .map(|(agent_id, status)| AgentRebindHint { agent_id, status })
        .collect()
}

pub fn summarize_text(text: &str, limit: usize) -> String {
    let take = limit.saturating_sub(3);
    let mut count = 0;
    let mut out = String::new();
    for ch in text.chars() {
        if count >= take {
            out.push_str("...");
            return out;
        }
        if ch.is_control() && ch != '\n' && ch != '\t' {
            continue;
        }
        out.push(ch);
        count += 1;
    }
    out
}

fn duration_ms(start: DateTime<Utc>, end: DateTime<Utc>) -> u64 {
    let millis = (end - start).num_milliseconds();
    if millis.is_negative() {
        0
    } else {
        u64::try_from(millis).unwrap_or(u64::MAX)
    }
}

fn panic_payload_message(payload: &(dyn std::any::Any + Send)) -> String {
    if let Some(message) = payload.downcast_ref::<&str>() {
        (*message).to_string()
    } else if let Some(message) = payload.downcast_ref::<String>() {
        message.clone()
    } else {
        "unknown panic payload".to_string()
    }
}

fn checked_runtime_store_root(root: PathBuf) -> Result<PathBuf> {
    if root.as_os_str().is_empty() {
        bail!("Runtime store root cannot be empty");
    }
    if root
        .components()
        .any(|component| matches!(component, Component::ParentDir))
    {
        bail!("Runtime store root cannot contain '..' components");
    }
    let absolute = if root.is_absolute() {
        root
    } else {
        std::env::current_dir()
            .context("failed to resolve current directory for runtime store")?
            .join(root)
    };
    match absolute.canonicalize() {
        Ok(path) => Ok(path),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            Ok(normalize_path_components(&absolute))
        }
        Err(err) => Err(err).with_context(|| {
            format!(
                "Failed to resolve runtime store root {}",
                absolute.display()
            )
        }),
    }
}

fn checked_existing_runtime_store_dir(path: &Path) -> Result<PathBuf> {
    reject_symlinked_store_dir(path)?;
    path.canonicalize()
        .with_context(|| format!("Failed to resolve {}", path.display()))
}

fn normalize_path_components(path: &Path) -> PathBuf {
    let mut normalized = PathBuf::new();
    for component in path.components() {
        match component {
            Component::Prefix(_) | Component::RootDir => normalized.push(component.as_os_str()),
            Component::CurDir => {}
            Component::ParentDir => {
                normalized.pop();
            }
            Component::Normal(part) => normalized.push(part),
        }
    }
    if normalized.as_os_str().is_empty() {
        PathBuf::from(".")
    } else {
        normalized
    }
}

fn reject_symlinked_store_file(path: &Path) -> Result<()> {
    let Ok(metadata) = fs::symlink_metadata(path) else {
        return Ok(());
    };
    if metadata.file_type().is_symlink() {
        bail!(
            "Runtime store file must not be a symlink: {}",
            path.display()
        );
    }
    Ok(())
}

fn reject_symlinked_store_dir(path: &Path) -> Result<()> {
    let Ok(metadata) = fs::symlink_metadata(path) else {
        return Ok(());
    };
    if metadata.file_type().is_symlink() {
        bail!(
            "Runtime store directory must not be a symlink: {}",
            path.display()
        );
    }
    if !metadata.is_dir() {
        bail!("Runtime store path must be a directory: {}", path.display());
    }
    Ok(())
}

fn ensure_runtime_store_dir(path: &Path) -> Result<()> {
    fs::create_dir_all(path).with_context(|| format!("Failed to create {}", path.display()))?;
    reject_symlinked_store_dir(path)
}

fn read_store_file(path: &Path) -> Result<String> {
    reject_symlinked_store_file(path)?;
    fs::read_to_string(path).with_context(|| format!("Failed to read {}", path.display()))
}

fn write_json_atomic<T: Serialize>(path: &Path, value: &T) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("Failed to create directory {}", parent.display()))?;
    }
    reject_symlinked_store_file(path)?;
    let payload = serde_json::to_string_pretty(value)?;
    crate::utils::write_atomic(path, payload.as_bytes())
        .with_context(|| format!("Failed to write {}", path.display()))
}

fn remove_file_if_exists(path: &Path) -> Result<()> {
    reject_symlinked_store_file(path)?;
    match fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(err) => Err(err).with_context(|| format!("Failed to remove {}", path.display())),
    }
}

#[cfg(test)]
mod tests;
