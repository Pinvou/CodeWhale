//! Session state management for the core engine.
//!
//! Tracks conversation history, token usage, and session metadata.

use crate::models::{Message, SystemPrompt, Usage};
use crate::prefix_cache::PrefixStabilityManager;
use crate::project_context::{ProjectContext, load_project_context_with_parents};
use crate::prompt_zones::{AppendLog, FrozenPrefix};
use crate::tui::approval::ApprovalMode;
use crate::working_set::WorkingSet;
use std::collections::{HashSet, VecDeque};
use std::path::PathBuf;

/// Maximum number of deferred schemas a conversation may keep in its active
/// toolbox. The permanent `read`/`write`/`edit`/`bash`/`agent`/`tool_search`
/// router surface is not counted here.
///
/// Pinvou fork: effectively unbounded. Upstream bounds the toolbox to 8
/// names / 16 KiB with silent LRU eviction; under provider prompt-cache
/// pricing every eviction mutates the request tools array and re-prefills the
/// entire prefix (write premiums included), which costs orders of magnitude
/// more than the ~0.1x cached-read saving it buys (codewhale-hq/Codewhale#6969),
/// and the live incident class was a model fabricating results for tools that
/// had silently vanished mid-task. This fork keeps the admission bookkeeping
/// and the catalog-shrink revalidation exactly as upstream — a disconnected
/// server, deny rule, mode switch, or eager promotion still drops entries —
/// but the size bounds never fire, so an activated schema stays callable for
/// the conversation's lifetime. `usize::MAX` itself cannot serve as the bound:
/// comparisons against the type extreme trip clippy's deny-by-default
/// `absurd_extreme_comparisons` (rustc's `unused_comparisons` joins in only
/// when the extreme is written as a numeric literal, not through a const
/// item), so the bound is an effectively-unbounded sentinel at
/// `usize::MAX / 2` — any slightly smaller value compiles; only the exact
/// extreme is lint-rejected.
pub(crate) const TOOL_ACTIVATION_CACHE_MAX_NAMES: usize = usize::MAX / 2;
/// Maximum serialized bytes added to requests by cached deferred schemas.
///
/// Pinvou fork: effectively unbounded; see [`TOOL_ACTIVATION_CACHE_MAX_NAMES`]
/// for why the bound is `usize::MAX / 2` rather than `usize::MAX`.
pub(crate) const TOOL_ACTIVATION_CACHE_MAX_SCHEMA_BYTES: usize = usize::MAX / 2;

/// Bounded, process-local conversation cache for deferred tools activated by
/// `tool_search` or loaded by a direct call (hydration, execution refresh,
/// retrieval admission).
///
/// Only names are retained. Every turn revalidates them against the currently
/// filtered catalog, so a disconnected MCP server, changed allow/deny rule, or
/// mode switch cannot resurrect a tool from an older authority posture. The
/// Pinvou fork disables the size bounds (see the constants above); upstream's
/// 8-name/16KiB LRU eviction no longer fires here.
#[derive(Debug, Clone, Default)]
pub(crate) struct ToolActivationCache {
    /// Least-recently used at the front, most-recently used at the back.
    names: VecDeque<String>,
}

/// Why `revalidate` dropped a cached activation as unavailable. The cause
/// selects the model-facing reason: a catalog-shrink name cannot come back
/// while the shrink holds, while an oversize name can return once its
/// schema shrinks back under the per-tool budget.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum ToolActivationStaleCause {
    /// The tool left the filtered catalog entirely (MCP disconnect, deny
    /// rule, mode switch).
    CatalogShrink,
    /// The tool is still in the catalog but its serialized definition
    /// (the whole `Tool`, not only its schema) exceeds the per-tool
    /// toolbox budget.
    Oversize,
}

/// One cached activation dropped by `revalidate` as unavailable. Eager
/// promotions are deliberately absent: the tool is directly callable and
/// stays active, so there is nothing to explain.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct ToolActivationStale {
    pub(crate) name: String,
    pub(crate) cause: ToolActivationStaleCause,
}

#[derive(Debug, Clone, Default, Eq, PartialEq)]
pub(crate) struct ToolActivationDelta {
    pub(crate) admitted: Vec<String>,
    /// Bounds-driven evictions: collective LRU or serialized-bytes overflow
    /// from this admission, or from revalidation after a cached schema grew
    /// past the budget. The tools are still deferred catalog members, so a
    /// later search can re-activate them — the "evicted" recovery contract.
    /// A name this same admission re-admitted is netted out, so reporting
    /// sites can quote the list verbatim without disclaiming a tool the same
    /// result hands back.
    pub(crate) evicted: Vec<String>,
    /// Unavailable entries dropped by `revalidate` before this admission:
    /// the tool left the deferred catalog or outgrew the per-tool budget,
    /// so it is unavailable rather than evicted. The cause selects the
    /// truthful model-facing reason.
    pub(crate) stale: Vec<ToolActivationStale>,
    pub(crate) rejected: Vec<String>,
}

impl ToolActivationDelta {
    /// Every name this delta dropped from the active toolbox: unavailable
    /// entries plus bounds evictions. Eager promotions are classified out
    /// by `revalidate` and never appear here, so removing these names can
    /// never hide a tool the catalog still exposes eagerly.
    pub(crate) fn dropped_names(&self) -> impl Iterator<Item = &str> {
        self.stale
            .iter()
            .map(|entry| entry.name.as_str())
            .chain(self.evicted.iter().map(String::as_str))
    }
}

/// Which step surfaced a toolbox eviction, so the model-facing note can
/// attribute it to the step that caused it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum ToolboxEvictionTrigger {
    /// A `tool_search` result naming the evictions in its own payload.
    ToolSearch,
    /// A non-search activation step: a hydration batch, a retrieval
    /// admission, or an executed tool's refresh.
    Step,
    /// The turn-entry revalidation.
    TurnStart,
}

impl ToolboxEvictionTrigger {
    pub(crate) fn label(self) -> &'static str {
        match self {
            Self::ToolSearch => "search",
            Self::Step => "step",
            Self::TurnStart => "turn's start",
        }
    }
}

/// A toolbox eviction parked for a later result to carry, with its names
/// still structured. A step's own touch/retrieval evictions pass through
/// the same struct at attach time so parked and step notes share one
/// netting and rendering path. Names stay unresolved until ride time so
/// the attaching channel can net out names the model can call again by
/// then: between the park and the ride, a search, a hydration, or a
/// retrieval may have re-admitted a victim, and a mid-turn MCP catalog
/// refresh can re-advertise one into the active set without re-activating
/// the cache. A note naming a callable tool "no longer callable" would
/// contradict the very result that hands it back.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct PendingToolboxEviction {
    pub(crate) trigger: ToolboxEvictionTrigger,
    pub(crate) evicted: Vec<String>,
}

/// What [`ToolActivationCache::revalidate`] removed, classified by
/// recoverability so callers can report each bucket truthfully.
#[derive(Debug, Clone, Default, Eq, PartialEq)]
pub(crate) struct ToolActivationRemovals {
    /// Unavailable drops: the tool left the deferred catalog or outgrew the
    /// per-tool budget. See [`ToolActivationStale`].
    pub(crate) stale: Vec<ToolActivationStale>,
    /// Collective-bounds victims that are still deferred catalog members:
    /// a later search can re-activate them.
    pub(crate) bounds_evicted: Vec<String>,
}

impl ToolActivationRemovals {
    /// Every name this removal set dropped from the active toolbox.
    pub(crate) fn dropped_names(&self) -> impl Iterator<Item = &str> {
        self.stale
            .iter()
            .map(|entry| entry.name.as_str())
            .chain(self.bounds_evicted.iter().map(String::as_str))
    }
}

impl ToolActivationCache {
    /// Forget every deferred tool activated by the current conversation.
    ///
    /// `Op::SyncSession` calls this before installing another conversation's
    /// identity, history, and workspace. The cache is intentionally
    /// process-local, but it must still be conversation-local.
    pub(crate) fn clear(&mut self) {
        self.names.clear();
    }

    fn catalog_tool<'a>(
        catalog: &'a [crate::models::Tool],
        name: &str,
    ) -> Option<&'a crate::models::Tool> {
        catalog
            .iter()
            .find(|tool| tool.name == name && tool.defer_loading.unwrap_or(false))
    }

    fn serialized_bytes(tool: &crate::models::Tool) -> usize {
        serde_json::to_vec(tool).map_or(usize::MAX, |bytes| bytes.len())
    }

    /// Whether a tool's serialized definition alone exceeds the per-tool
    /// toolbox budget, so no activation can admit it. The measurement is
    /// the whole serialized `Tool` — name, description, schema, and every
    /// set optional — matching the collective budget's unit. Shared by
    /// admission, revalidation, and hydration planning so the gate cannot
    /// drift.
    pub(crate) fn exceeds_per_tool_budget(tool: &crate::models::Tool) -> bool {
        Self::serialized_bytes(tool) > TOOL_ACTIVATION_CACHE_MAX_SCHEMA_BYTES
    }

    fn total_serialized_bytes(&self, catalog: &[crate::models::Tool]) -> usize {
        self.names
            .iter()
            .filter_map(|name| Self::catalog_tool(catalog, name))
            .map(Self::serialized_bytes)
            .fold(0usize, usize::saturating_add)
    }

    /// Pop least-recently-used entries until both collective bounds hold.
    /// Shared by revalidation and admission so the bounds condition and the
    /// LRU-front-first policy live in exactly one place.
    fn pop_collective_overflow(&mut self, catalog: &[crate::models::Tool]) -> Vec<String> {
        let mut popped = Vec::new();
        while self.names.len() > TOOL_ACTIVATION_CACHE_MAX_NAMES
            || self.total_serialized_bytes(catalog) > TOOL_ACTIVATION_CACHE_MAX_SCHEMA_BYTES
        {
            let Some(name) = self.names.pop_front() else {
                break;
            };
            popped.push(name);
        }
        popped
    }

    /// Drop entries that are no longer deferred members of this turn's
    /// filtered catalog and enforce both cache bounds, classifying every
    /// removal by recoverability. A tool that became eager is forgotten
    /// from the cache without being reported: it is directly callable, and
    /// the eager catalog entry must stay active.
    pub(crate) fn revalidate(&mut self, catalog: &[crate::models::Tool]) -> ToolActivationRemovals {
        let mut removals = ToolActivationRemovals::default();
        self.names.retain(|name| {
            let Some(tool) = catalog.iter().find(|tool| tool.name == *name) else {
                removals.stale.push(ToolActivationStale {
                    name: name.clone(),
                    cause: ToolActivationStaleCause::CatalogShrink,
                });
                return false;
            };
            if !tool.defer_loading.unwrap_or(false) {
                return false;
            }
            if Self::exceeds_per_tool_budget(tool) {
                removals.stale.push(ToolActivationStale {
                    name: name.clone(),
                    cause: ToolActivationStaleCause::Oversize,
                });
                return false;
            }
            true
        });
        removals.bounds_evicted = self.pop_collective_overflow(catalog);
        removals
            .stale
            .sort_by(|left, right| left.name.cmp(&right.name));
        removals.stale.dedup();
        removals.bounds_evicted.sort();
        removals.bounds_evicted.dedup();
        removals
    }

    /// Touch requested deferred tools in search-result order. Upstream
    /// rejects oversized schemas and evicts least-recently-used entries until
    /// both size bounds hold; the Pinvou fork disables those bounds (see the
    /// constants above), so those paths never fire and every request naming a
    /// deferred catalog member is admitted.
    pub(crate) fn activate(
        &mut self,
        catalog: &[crate::models::Tool],
        requested: &[String],
    ) -> ToolActivationDelta {
        let revalidated = self.revalidate(catalog);
        let mut delta = ToolActivationDelta {
            stale: revalidated.stale,
            evicted: revalidated.bounds_evicted,
            ..ToolActivationDelta::default()
        };
        let mut seen = HashSet::new();
        for name in requested {
            if !seen.insert(name.clone()) {
                continue;
            }
            let Some(tool) = Self::catalog_tool(catalog, name) else {
                delta.rejected.push(name.clone());
                continue;
            };
            if Self::exceeds_per_tool_budget(tool) {
                delta.rejected.push(name.clone());
                continue;
            }
            if let Some(index) = self.names.iter().position(|cached| cached == name) {
                self.names.remove(index);
            }
            self.names.push_back(name.clone());
            delta.evicted.extend(self.pop_collective_overflow(catalog));
        }

        let retained = self.names.iter().collect::<HashSet<_>>();
        delta.admitted = requested
            .iter()
            .filter(|name| retained.contains(name))
            .cloned()
            .collect();
        // A revalidation pop of a name this same activation re-admits (the
        // searched tool was the LRU-front victim of a growth overflow, for
        // example) is not an eviction: the result that names it evicted also
        // hands it back. Net it out so no report lies about it.
        delta.evicted.retain(|name| !delta.admitted.contains(name));
        delta.evicted.sort();
        delta.evicted.dedup();
        delta
            .stale
            .sort_by(|left, right| left.name.cmp(&right.name));
        delta.stale.dedup();
        delta.rejected.sort();
        delta.rejected.dedup();
        delta
    }

    pub(crate) fn names(&self) -> impl Iterator<Item = &str> {
        self.names.iter().map(String::as_str)
    }

    /// Whether `name` is currently cached, and thus callable again.
    pub(crate) fn contains(&self, name: &str) -> bool {
        self.names.iter().any(|cached| cached == name)
    }
}

/// Session state for the engine.
#[derive(Debug, Clone)]
pub struct Session {
    /// Model being used
    pub model: String,

    /// Reasoning-effort tier for DeepSeek thinking mode:
    /// `"off" | "low" | "medium" | "high" | "max"`. `None` lets the provider
    /// apply its own defaults.
    pub reasoning_effort: Option<String>,
    /// Whether the user selected automatic reasoning effort.
    pub reasoning_effort_auto: bool,

    /// Whether the user selected automatic model routing.
    pub auto_model: bool,

    /// Workspace directory
    pub workspace: PathBuf,

    /// Additional workspace roots for this session; `workspace` is always the
    /// primary root. Read on every turn when the sandbox policy, tool
    /// boundary, and exec-policy context are materialized, so a root-set
    /// change takes effect on the next turn. Empty means single-root.
    pub workspace_roots: Vec<PathBuf>,

    /// System prompt (optional)
    pub system_prompt: Option<SystemPrompt>,
    /// True when `system_prompt` is a persisted/runtime-supplied prefix that
    /// should not be replaced by mode/context refreshes.
    pub system_prompt_override: bool,
    /// Hash of the last assembled stable system prompt. Used to avoid
    /// replacing `system_prompt` when unchanged.
    pub last_system_prompt_hash: Option<u64>,
    /// Reason the pinned prefix will move on the next model request, set by
    /// an explicit header-change op (`/model`, mode change, goal edit, session
    /// sync) when it actually alters the system-prompt bytes. Consumed by the
    /// turn loop's prefix check so a declared change re-pins under a logged
    /// reason while an undeclared mid-loop change is reported as drift and the
    /// pin holds. `None` means "no declared change since the last request".
    pub pending_prefix_change_reason: Option<String>,
    /// Evictions parked by non-search activations (a batch's hydration of
    /// directly-called deferred tools, the turn-entry revalidation) that
    /// have no result payload of their own. The next recorded tool result
    /// carries them to the model; names a ride re-admits are netted out at
    /// attach time because the model can call them again. Taken and cleared
    /// by the turn loop when the notes are attached.
    pub(super) pending_toolbox_eviction_notes: Vec<PendingToolboxEviction>,
    /// The explicit prompt inputs (model, mode, goal, route, translation,
    /// verbosity) the pinned system prompt was composed from. At each new user
    /// turn the engine recomposes: if these inputs are unchanged but the
    /// composed bytes differ, that is workspace/instruction/skills/memory
    /// drift and is delivered to the model as a `<context_update>` message,
    /// never by moving the pinned header.
    pub(crate) pinned_prompt_context: Option<crate::core::engine::NextTurnPromptContext>,
    /// Flat text of the session context the model has last been shown, either
    /// as the pinned header or through `<context_update>` messages. New-turn
    /// deltas are computed against this so an update is delivered exactly once.
    pub(crate) context_update_baseline: Option<String>,
    /// Host-persistence copy of the history checkpoint generated by context
    /// compaction. This is never part of the standing system prompt.
    pub compaction_summary_prompt: Option<SystemPrompt>,

    /// Conversation history (API format), backed by AppendLog (#2264).
    pub messages: AppendLog,

    /// Total tokens used in this session
    pub total_usage: SessionUsage,

    /// Whether shell execution is allowed
    pub allow_shell: bool,

    /// Whether to trust paths outside workspace
    pub trust_mode: bool,

    /// Whether the current session should auto-approve tool safety checks.
    pub auto_approve: bool,

    /// Live UI approval policy used to steer the system prompt.
    pub approval_mode: ApprovalMode,

    /// Notes file path
    pub notes_path: PathBuf,

    /// MCP config path
    pub mcp_config_path: PathBuf,

    /// Session ID (for tracking)
    pub id: String,

    /// Project context loaded from AGENTS.md, etc.
    pub project_context: Option<ProjectContext>,

    /// Repo-aware working set for context management.
    pub working_set: WorkingSet,

    /// Prefix-cache stability monitor (inspired by Reasonix's Pillar 1).
    /// Tracks the immutable prefix fingerprint and detects drift across turns.
    /// Set during engine construction; None until the first system prompt assembly.
    pub prefix_stability: Option<PrefixStabilityManager>,

    /// Three-zone immutable prefix baseline (#2264). Frozen on the first
    /// request of the session; verified against the current system+tool
    /// state before every subsequent request. None until the first turn.
    pub frozen_prefix: Option<FrozenPrefix>,

    /// Deferred tools explicitly discovered during this conversation. Names
    /// are revalidated against the live catalog before each request.
    pub(super) tool_activation_cache: ToolActivationCache,

    /// Monotonic counter bumped on every direct mutation of `messages`.
    /// Consumed by the engine token-estimate cache
    /// to memoize the per-turn token estimate without re-walking the message
    /// list. Defaults to 0; bumped in [`Session::add_message`],
    /// [`Session::replace_messages`], and at other mutation sites in
    /// `core/engine.rs`.
    pub messages_revision: u64,

    /// Provider-billed prompt tokens from the most recent parent-route
    /// request that still describes the live message list. Survives turn
    /// boundaries so the next send can compact on last-turn pressure
    /// (#5577). Cleared when history is rewritten (compaction, restore).
    pub(crate) latest_parent_input_tokens: Option<u32>,
}

/// Cumulative usage statistics for a session.
#[derive(Debug, Clone, Default)]
#[allow(clippy::struct_field_names)]
pub struct SessionUsage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    /// Cache creation (write) tokens. `None` when never observed by the API —
    /// do NOT display as 0, which would be indistinguishable from "no writes".
    pub cache_creation_input_tokens: Option<u64>,
    /// Cache read (hit) tokens. `None` when never observed by the API —
    /// do NOT display as 0, which would be indistinguishable from "no hits".
    pub cache_read_input_tokens: Option<u64>,
}

impl SessionUsage {
    /// Add usage from a turn
    pub fn add(&mut self, usage: &Usage) {
        self.input_tokens += u64::from(usage.input_tokens);
        self.output_tokens += u64::from(usage.output_tokens);
        if let Some(tokens) = usage.prompt_cache_write_tokens {
            self.cache_creation_input_tokens =
                Some(self.cache_creation_input_tokens.unwrap_or(0) + u64::from(tokens));
        }
        if let Some(tokens) = usage.prompt_cache_hit_tokens {
            self.cache_read_input_tokens =
                Some(self.cache_read_input_tokens.unwrap_or(0) + u64::from(tokens));
        }
    }
}

impl Session {
    /// Create a new session
    pub fn new(
        model: String,
        workspace: PathBuf,
        allow_shell: bool,
        trust_mode: bool,
        notes_path: PathBuf,
        mcp_config_path: PathBuf,
    ) -> Self {
        // Load project context from AGENTS.md, CLAUDE.md, etc.
        let project_context = load_project_context_with_parents(&workspace);
        let has_context = project_context.has_instructions();

        Self {
            model,
            reasoning_effort: None,
            reasoning_effort_auto: false,
            auto_model: false,
            workspace,
            workspace_roots: Vec::new(),
            system_prompt: None,
            system_prompt_override: false,
            compaction_summary_prompt: None,
            messages: AppendLog::new(),
            total_usage: SessionUsage::default(),
            allow_shell,
            trust_mode,
            auto_approve: false,
            approval_mode: ApprovalMode::Suggest,
            notes_path,
            mcp_config_path,
            id: uuid::Uuid::new_v4().to_string(),
            project_context: if has_context {
                Some(project_context)
            } else {
                None
            },
            last_system_prompt_hash: None,
            pending_prefix_change_reason: None,
            pending_toolbox_eviction_notes: Vec::new(),
            pinned_prompt_context: None,
            context_update_baseline: None,
            working_set: WorkingSet::default(),
            prefix_stability: None,
            frozen_prefix: None,
            tool_activation_cache: ToolActivationCache::default(),
            messages_revision: 0,
            latest_parent_input_tokens: None,
        }
    }

    /// Add a message to the conversation
    pub fn add_message(&mut self, message: Message) {
        self.messages.push(message);
        self.messages_revision = self.messages_revision.saturating_add(1);
    }

    /// Replace the entire message history. Used by session resume and
    /// compaction. Bumps `messages_revision` exactly once even when the new
    /// history has a different length, so downstream caches invalidate
    /// atomically.
    #[allow(dead_code)]
    pub fn replace_messages(&mut self, messages: Vec<Message>) {
        self.messages = messages.into();
        self.messages_revision = self.messages_revision.saturating_add(1);
        self.latest_parent_input_tokens = None;
    }

    /// Bump `messages_revision` without otherwise mutating the message list.
    /// Reserved for sites that mutate the message list in place (e.g. an
    /// in-place rewrite of a content block). Most call sites do not need
    /// this — prefer [`add_message`](Self::add_message) and
    /// [`replace_messages`](Self::replace_messages).
    pub fn bump_messages_revision(&mut self) {
        self.messages_revision = self.messages_revision.saturating_add(1);
    }

    /// Rebuild the working set from current messages (best effort).
    pub fn rebuild_working_set(&mut self) {
        self.working_set
            .rebuild_from_messages(&self.messages, &self.workspace);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn deferred_tool(name: &str, description_bytes: usize) -> crate::models::Tool {
        crate::models::Tool {
            tool_type: None,
            name: name.to_string(),
            description: "x".repeat(description_bytes),
            input_schema: json!({"type": "object", "properties": {}}),
            allowed_callers: None,
            defer_loading: Some(true),
            input_examples: None,
            strict: None,
            cache_control: None,
        }
    }

    #[test]
    fn session_usage_cache_starts_none() {
        let usage = SessionUsage::default();
        assert!(usage.cache_creation_input_tokens.is_none());
        assert!(usage.cache_read_input_tokens.is_none());
    }

    #[test]
    fn session_usage_cache_remains_none_when_api_omits_cache() {
        let mut usage = SessionUsage::default();
        let api_usage = Usage {
            input_tokens: 100,
            output_tokens: 50,
            prompt_cache_hit_tokens: None,
            prompt_cache_miss_tokens: None,
            prompt_cache_write_tokens: None,
            reasoning_tokens: None,
            reasoning_replay_tokens: None,
            server_tool_use: None,
        };
        usage.add(&api_usage);
        assert!(usage.cache_creation_input_tokens.is_none());
        assert!(usage.cache_read_input_tokens.is_none());
    }

    #[test]
    fn session_usage_cache_accumulates_when_reported() {
        let mut usage = SessionUsage::default();
        let api_usage = Usage {
            input_tokens: 100,
            output_tokens: 50,
            prompt_cache_hit_tokens: Some(30),
            prompt_cache_miss_tokens: Some(50),
            prompt_cache_write_tokens: Some(20),
            reasoning_tokens: None,
            reasoning_replay_tokens: None,
            server_tool_use: None,
        };
        usage.add(&api_usage);
        assert_eq!(usage.cache_read_input_tokens, Some(30));
        assert_eq!(usage.cache_creation_input_tokens, Some(20));
        usage.add(&api_usage);
        assert_eq!(usage.cache_read_input_tokens, Some(60));
        assert_eq!(usage.cache_creation_input_tokens, Some(40));
    }

    #[test]
    fn session_usage_cache_preserves_explicit_zero() {
        let mut usage = SessionUsage::default();
        let api_usage = Usage {
            input_tokens: 100,
            output_tokens: 50,
            prompt_cache_hit_tokens: Some(0), // explicit zero from provider
            prompt_cache_miss_tokens: Some(50),
            prompt_cache_write_tokens: Some(1234),
            reasoning_tokens: None,
            reasoning_replay_tokens: None,
            server_tool_use: None,
        };
        usage.add(&api_usage);
        // 0 is a valid observed value, must NOT be converted to None
        assert_eq!(usage.cache_read_input_tokens, Some(0));
        assert_eq!(usage.cache_creation_input_tokens, Some(1234));
    }

    #[test]
    fn forkguard_deferred_toolbox_bounds_are_disabled() {
        // The Pinvou fork disables the bounded-toolbox eviction (see the
        // constant docs): activating far more names than upstream's historical
        // 8-name bound must retain every admission and evict nothing. The
        // 1 KiB schemas also push the total past upstream's historical 16 KiB
        // byte budget, so a byte-bound-only revert fails here too.
        let catalog = (0..64)
            .map(|index| deferred_tool(&format!("tool_{index}"), 1024))
            .collect::<Vec<_>>();
        let requested = catalog
            .iter()
            .map(|tool| tool.name.clone())
            .collect::<Vec<_>>();
        let mut cache = ToolActivationCache::default();
        let delta = cache.activate(&catalog, &requested);

        assert_eq!(cache.names().count(), 64);
        assert_eq!(delta.admitted, requested);
        assert!(delta.evicted.is_empty());
        assert!(delta.rejected.is_empty());
    }

    #[test]
    fn forkguard_oversized_schema_is_admitted_while_the_bounds_are_disabled() {
        // 17 KiB exceeds upstream's historical 16 KiB per-tool bound; with the
        // bounds disabled it is admitted like any other schema. The literal is
        // sized against the historical bound because no constructible schema
        // can approach the fork bound (usize::MAX / 2).
        let catalog = vec![deferred_tool("huge", 17 * 1024)];
        let mut cache = ToolActivationCache::default();
        let delta = cache.activate(&catalog, &["huge".to_string()]);

        assert_eq!(cache.names().count(), 1);
        assert_eq!(delta.admitted, vec!["huge".to_string()]);
        assert!(delta.rejected.is_empty());
        assert!(delta.evicted.is_empty());
    }

    #[test]
    fn forkguard_touching_a_cached_tool_moves_it_to_the_back_and_evicts_nothing() {
        let catalog = (0..9)
            .map(|index| deferred_tool(&format!("tool_{index}"), 8))
            .collect::<Vec<_>>();
        let first_eight = catalog[..8]
            .iter()
            .map(|tool| tool.name.clone())
            .collect::<Vec<_>>();
        let mut cache = ToolActivationCache::default();
        cache.activate(&catalog, &first_eight);
        cache.activate(&catalog, &["tool_0".to_string()]);
        cache.activate(&catalog, &["tool_8".to_string()]);

        let names = cache.names().collect::<Vec<_>>();
        // The recency order is still maintained (most recent at the back), but
        // with the bounds disabled the touched-then-re-added tool no longer
        // displaces the least-recently-used entry.
        assert_eq!(names.len(), 9);
        assert!(names.contains(&"tool_0"));
        assert!(names.contains(&"tool_1"));
        assert_eq!(names.last().copied(), Some("tool_8"));
    }
    #[test]
    fn revalidate_classifies_catalog_shrink_and_eager_removals() {
        let catalog = vec![
            deferred_tool("kept", 8),
            deferred_tool("disconnected", 8),
            deferred_tool("promoted", 8),
        ];
        let mut cache = ToolActivationCache::default();
        cache.activate(
            &catalog,
            &[
                "kept".to_string(),
                "disconnected".to_string(),
                "promoted".to_string(),
            ],
        );
        let mut next_catalog = vec![deferred_tool("kept", 8), deferred_tool("promoted", 8)];
        next_catalog[1].defer_loading = Some(false);

        let removals = cache.revalidate(&next_catalog);
        assert_eq!(cache.names().collect::<Vec<_>>(), vec!["kept"]);
        assert_eq!(
            removals.stale,
            vec![ToolActivationStale {
                name: "disconnected".to_string(),
                cause: ToolActivationStaleCause::CatalogShrink,
            }]
        );
        // The eager promotion is forgotten from the cache without being
        // reported: the tool is directly callable, so both the unavailable
        // and the evicted vocabulary would be false statements.
        assert!(removals.bounds_evicted.is_empty());
    }
    #[test]
    fn clearing_for_session_sync_forgets_all_activated_tools() {
        let catalog = vec![deferred_tool("one", 8), deferred_tool("two", 8)];
        let mut cache = ToolActivationCache::default();
        cache.activate(&catalog, &["one".to_string(), "two".to_string()]);

        cache.clear();

        assert_eq!(cache.names().count(), 0);
    }
}
