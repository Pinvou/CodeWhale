//! Operations submitted by the UI to the core engine.
//!
//! These operations flow from the TUI to the engine via a channel,
//! allowing the UI to remain responsive while the engine processes requests.

use crate::compaction::CompactionConfig;
use crate::config::ApiProvider;
use crate::models::{Message, SystemPrompt};
use crate::route_runtime::ResolvedRuntimeRoute;
use crate::tools::goal::GoalStatus;
use crate::tui::app::AppMode;
use crate::tui::approval::ApprovalMode;
use codewhale_protocol::runtime::DynamicToolSpec;
use std::path::PathBuf;
use std::sync::Arc;

/// Prefix used for tool-call ids created by local composer shell shortcuts.
pub const USER_SHELL_TOOL_ID_PREFIX: &str = "user_shell_";

/// Process-local, exact tool-dispatch authority for an embedded turn.
///
/// This deliberately does not implement serde traits: a host must install the
/// authority in-process for each turn rather than accepting it from a saved
/// transcript or another untrusted serialization boundary.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ExactToolDispatchPolicy {
    allowed: Arc<[String]>,
}

impl ExactToolDispatchPolicy {
    pub fn try_new(names: impl IntoIterator<Item = String>) -> Result<Self, String> {
        let mut allowed = Vec::new();
        let mut seen = std::collections::HashSet::new();
        for name in names {
            if name.is_empty() || name.trim() != name || name.contains('*') || name.contains('?') {
                return Err("invalid exact tool name".to_string());
            }
            if name == "agent"
                || name == "start_mcp_server"
                || crate::mcp::McpPool::is_mcp_tool(&name)
            {
                return Err(
                    "control-plane and MCP tools are not valid exact dispatch names".into(),
                );
            }
            if !seen.insert(name.clone()) {
                return Err("duplicate exact tool name".to_string());
            }
            allowed.push(name);
        }
        Ok(Self {
            allowed: allowed.into(),
        })
    }

    pub fn allowed_tools(&self) -> &[String] {
        &self.allowed
    }

    pub fn allows(&self, canonical_name: &str) -> bool {
        self.allowed.iter().any(|allowed| allowed == canonical_name)
    }
}

/// Optional turn-scoped hardening supplied by an embedding host.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TurnToolSecurityPolicy {
    trusted_external_paths_override: Option<Arc<[PathBuf]>>,
    exact_dispatch: Option<ExactToolDispatchPolicy>,
    allow_hooks: bool,
}

impl TurnToolSecurityPolicy {
    pub fn new(
        // Trusted roots must be canonical absolute paths. The host owns
        // canonicalization because this process-local policy never performs
        // filesystem I/O during construction.
        trusted_external_paths_override: Option<Vec<PathBuf>>,
        exact_dispatch: Option<ExactToolDispatchPolicy>,
    ) -> Self {
        Self {
            trusted_external_paths_override: trusted_external_paths_override.map(Into::into),
            exact_dispatch,
            allow_hooks: false,
        }
    }

    pub fn trusted_external_paths_override(&self) -> Option<&[PathBuf]> {
        self.trusted_external_paths_override.as_deref()
    }

    pub fn exact_dispatch(&self) -> Option<&ExactToolDispatchPolicy> {
        self.exact_dispatch.as_ref()
    }

    /// Explicitly permit the embedding host's hooks for this restricted turn.
    /// Restricted policies default to no hooks because hook executors may
    /// launch external processes and receive the full tool input.
    pub fn with_trusted_hooks(mut self) -> Self {
        self.allow_hooks = true;
        self
    }

    pub fn allows_hooks(&self) -> bool {
        self.allow_hooks
    }
}

#[cfg(test)]
mod turn_security_tests {
    use super::*;

    #[test]
    fn restricted_turn_hooks_require_explicit_host_opt_in() {
        let default_policy = TurnToolSecurityPolicy::new(Some(Vec::new()), None);
        assert!(!default_policy.allows_hooks());

        let trusted = default_policy.with_trusted_hooks();
        assert!(trusted.allows_hooks());
    }
}

/// Snapshot of session state for saving to disk.
/// Returned by `Op::GetSessionSnapshot` via a oneshot channel.
#[derive(Debug, Clone)]
pub struct SessionSnapshot {
    pub messages: Vec<Message>,
    pub total_tokens: u64,
    pub model: String,
    /// Generic provider kind retained for serialized compatibility.
    pub model_provider: String,
    /// Exact non-secret configured provider key.
    pub model_provider_id: Option<String>,
    pub workspace: PathBuf,
    pub system_prompt: Option<SystemPrompt>,
    pub mode: String,
}

/// Provider request runtime state surfaced by `/provider`.
/// Returned by `Op::GetProviderRuntimeStatus` via a oneshot channel.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProviderRuntimeStatus {
    pub provider: ApiProvider,
    pub request_concurrency_limit: Option<usize>,
    pub active_provider_requests: usize,
}

/// Result of rebuilding the engine-owned MCP pool in process.
pub type McpReloadResult = Result<crate::mcp::McpManagerSnapshot, String>;

/// Origin of text being introduced as a user-role turn.
///
/// Chat providers force several runtime/control-plane signals through
/// `role = "user"` for compatibility, so role alone is not authority.
#[allow(dead_code)] // Some origins are reserved for ingestion sites landing after the first gate.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UserInputProvenance {
    /// Text typed or submitted through the active UI/API input boundary.
    ExternalUser,
    /// Runtime-generated continuation, diagnostic, or tool feedback.
    Runtime,
    /// Completion/event text from a child worker or sub-agent handoff.
    SubAgentHandoff,
    /// Text restored from a saved/imported transcript.
    ImportedTranscript,
    /// Text recalled from memory or another persisted source.
    MemoryRecall,
    /// Assistant-authored text that is shaped like a user response.
    AssistantGenerated,
}

impl UserInputProvenance {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::ExternalUser => "external_user",
            Self::Runtime => "runtime",
            Self::SubAgentHandoff => "subagent_handoff",
            Self::ImportedTranscript => "imported_transcript",
            Self::MemoryRecall => "memory_recall",
            Self::AssistantGenerated => "assistant_generated",
        }
    }

    pub fn can_authorize_work(self) -> bool {
        matches!(self, Self::ExternalUser)
    }
}

/// Operations that can be submitted to the engine.
#[derive(Debug)]
pub enum Op {
    /// Send a message to the AI
    SendMessage {
        content: String,
        mode: AppMode,
        /// Exact, structurally resolved route authority for this turn. The
        /// engine activates its client before mutating turn state; injected
        /// engines may use their already-supplied client with the same receipt.
        route: Box<ResolvedRuntimeRoute>,
        /// Compaction policy derived from the same provider route. Carrying it
        /// atomically avoids a model/limit mismatch before `SendMessage`.
        compaction: Box<CompactionConfig>,
        goal_objective: Option<String>,
        goal_token_budget: Option<u32>,
        goal_status: GoalStatus,
        /// Reasoning-effort tier: `"off" | "low" | "medium" | "high" | "max"`.
        /// `None` lets the provider apply its default.
        reasoning_effort: Option<String>,
        /// True when the user selected auto thinking, even though the UI sends
        /// a concrete per-turn value to the model API.
        reasoning_effort_auto: bool,
        /// True when the user selected auto model routing.
        auto_model: bool,
        allow_shell: bool,
        trust_mode: bool,
        auto_approve: bool,
        approval_mode: ApprovalMode,
        translation_enabled: bool,
        /// Tool restriction from custom slash command frontmatter.
        /// `None` means the current turn may use the normal tool set.
        allowed_tools: Option<Vec<String>>,
        /// Runtime-supplied tools available only for this turn.
        dynamic_tools: Vec<DynamicToolSpec>,
        /// Hook executor for control-plane hooks.
        /// `ToolCallBefore` hooks may deny a tool call with exit code 2.
        hook_executor: Option<std::sync::Arc<crate::hooks::HookExecutor>>,
        verbosity: Option<String>,
        /// Structural input origin. This gates whether the turn may inherit
        /// YOLO/auto-approval authority; user-shaped text is not enough.
        provenance: UserInputProvenance,
        /// Optional process-local hardening for this turn. `None` preserves
        /// the configured engine default and all legacy behavior.
        turn_tool_security: Option<Arc<TurnToolSecurityPolicy>>,
    },

    /// Re-check and dispatch an interactive goal continuation when this
    /// operation reaches the front of the engine queue. Keeping this distinct
    /// from `SendMessage` prevents a queued `/goal pause` or `/goal clear`
    /// from being overwritten by a stale synthetic Active snapshot.
    ContinueGoal {
        /// Runtime-supplied tools remain available across the synthetic turn
        /// that continues the same logical goal run.
        dynamic_tools: Vec<DynamicToolSpec>,
        /// Opaque identity for an engine-owned synthetic continuation. Direct
        /// callers use `None`; the engine uses `Some` to coalesce one token
        /// across capacity-waiting, enqueued, and running-adjacent states.
        engine_schedule_id: Option<u64>,
    },

    /// Execute a user-submitted composer shell command (`! <command>`) without
    /// sending a model turn. This still routes through `exec_shell`, approval,
    /// sandbox, and command-safety handling.
    RunShellCommand {
        command: String,
        mode: AppMode,
        allow_shell: bool,
        trust_mode: bool,
        auto_approve: bool,
        approval_mode: ApprovalMode,
    },

    /// Set the runtime goal status without dispatching a model turn. Used by
    /// `/goal pause`, `/goal resume`, `/goal clear`, etc. so the engine's
    /// `SharedGoalState` learns the new status immediately and a queued
    /// continuation doesn't overwrite it back to Active.
    SetGoalStatus {
        status: GoalStatus,
        /// When `true`, clear the objective entirely (`/goal clear`).
        clear: bool,
    },

    /// Cancel the current request
    #[allow(dead_code)]
    CancelRequest,

    /// Approve a tool call that requires permission
    #[allow(dead_code)]
    ApproveToolCall { id: String },

    /// Deny a tool call that requires permission
    #[allow(dead_code)]
    DenyToolCall { id: String },

    /// Spawn a sub-agent
    #[allow(dead_code)]
    SpawnSubAgent { prompt: String },

    /// Cancel every currently running sub-agent for this engine.
    ///
    /// Hosts use this during session cancellation and engine shutdown so no
    /// child work survives after its owning turn has been reclaimed.
    CancelSubAgents,

    /// Describe the exact request the next turn would send, without
    /// sending it (`/preview-request`, #1004).
    ///
    /// Handled by the engine because only the engine can rebuild the current
    /// tool catalog, MCP state, mode, gates, permission posture, and resolved
    /// route. Pure inspection: it adds no message, no turn, and no tool call.
    PreviewOutboundRequest {
        inputs: Box<crate::core::engine::preview::PreviewRequestInputs>,
        /// Render the manifest as JSON instead of the human-readable table.
        json: bool,
        /// Explicit disclosure of the base prompt only; effective system text
        /// remains protected behind hashes.
        base_prompt_only: bool,
    },

    /// List current sub-agents and their status
    ListSubAgents,

    /// Cancel a running sub-agent by id or session name.
    CancelSubAgent { agent_id: String },

    /// Change the operating mode
    #[allow(dead_code)]
    ChangeMode {
        mode: AppMode,
        allow_shell: bool,
        trust_mode: bool,
        auto_approve: bool,
        approval_mode: ApprovalMode,
        configured_sandbox_mode: Option<String>,
    },

    /// Update the model being used and refresh stable prompt context.
    #[allow(dead_code)]
    SetModel {
        model: String,
        mode: AppMode,
        route_limits: Option<codewhale_config::route::RouteLimits>,
    },

    /// Update auto-compaction settings
    SetCompaction { config: CompactionConfig },

    /// Replace the live user permission rules without clearing session-only
    /// approvals.
    SetPermissionRuleset {
        ruleset: codewhale_execpolicy::Ruleset,
    },

    /// Update the SSE idle timeout used for subsequent streamed turns.
    SetStreamChunkTimeout { timeout_secs: u64 },

    /// Replace the session-scoped model-facing tool deny-list.
    SetDisallowedTools { tools: Vec<String> },

    /// Update sub-agent runtime controls for subsequent turns.
    SetSubagentRuntimeConfig {
        enabled: bool,
        max_subagents: usize,
        launch_concurrency: usize,
        max_spawn_depth: u32,
        api_timeout_secs: u64,
        heartbeat_timeout_secs: u64,
    },

    /// Replace the engine's merged Fleet roster after the setup wizard saves a
    /// project or personal profile. Subsequent turns can use the new role
    /// immediately instead of requiring an application restart.
    SetFleetRoster {
        roster: std::sync::Arc<crate::fleet::roster::FleetRoster>,
    },

    /// Sync engine session state (used for resume/load)
    SyncSession {
        session_id: Option<String>,
        messages: Vec<Message>,
        system_prompt: Option<SystemPrompt>,
        system_prompt_override: bool,
        model: String,
        workspace: PathBuf,
        mode: AppMode,
    },

    /// Run context compaction on one exact, structurally resolved provider
    /// route with policy derived from that same descriptor.
    CompactContext {
        route: Box<ResolvedRuntimeRoute>,
        compaction: Box<CompactionConfig>,
    },

    /// Get a snapshot of the current session state (messages, tokens, etc.)
    /// for saving to disk. Returns the result via the oneshot sender so
    /// the caller doesn't have to compete with the SSE event stream.
    GetSessionSnapshot {
        tx: std::sync::Arc<std::sync::Mutex<Option<tokio::sync::oneshot::Sender<SessionSnapshot>>>>,
    },

    /// Get active provider request concurrency state for readiness surfaces.
    GetProviderRuntimeStatus {
        tx: std::sync::Arc<
            std::sync::Mutex<Option<tokio::sync::oneshot::Sender<ProviderRuntimeStatus>>>,
        >,
    },

    /// Force the engine-owned MCP config/catalog to reload and reconnect.
    /// The returned snapshot is taken from that same live pool.
    ReloadMcp {
        config_path: PathBuf,
        tx: std::sync::Arc<std::sync::Mutex<Option<tokio::sync::oneshot::Sender<McpReloadResult>>>>,
    },

    /// Run agent-driven context purging.
    PurgeContext,

    /// Edit the last user message: remove the last user+assistant exchange
    /// from the session, then re-send with the new content.
    #[allow(dead_code)]
    EditLastTurn { new_message: String },

    /// Enable or disable the background advisor watcher for this session.
    /// When enabled, a fire-and-forget background task runs after each turn
    /// that contained tool calls and emits an `Event::AdvisoryNote` with
    /// concise observations. (#3982)
    SetAdvisorEnabled { enabled: bool },

    /// Shutdown the engine
    Shutdown,
}

#[cfg(test)]
mod turn_tool_security_tests {
    use super::{ExactToolDispatchPolicy, TurnToolSecurityPolicy};
    use std::path::PathBuf;

    #[test]
    fn exact_tool_dispatch_policy_rejects_ambiguous_names_and_accepts_empty() {
        assert!(ExactToolDispatchPolicy::try_new(Vec::<String>::new()).is_ok());
        for names in [
            vec!["".to_string()],
            vec![" read_file".to_string()],
            vec!["read_file ".to_string()],
            vec!["read_*".to_string()],
            vec!["read?file".to_string()],
            vec!["read_file".to_string(), "read_file".to_string()],
            vec!["agent".to_string()],
            vec!["start_mcp_server".to_string()],
            vec!["mcp__server__tool".to_string()],
        ] {
            assert!(ExactToolDispatchPolicy::try_new(names).is_err());
        }
    }

    #[test]
    fn turn_tool_security_policy_keeps_trusted_path_override_tristate() {
        let legacy = TurnToolSecurityPolicy::new(None, None);
        assert!(legacy.trusted_external_paths_override().is_none());
        let empty = TurnToolSecurityPolicy::new(Some(Vec::new()), None);
        assert_eq!(empty.trusted_external_paths_override(), Some(&[][..]));
        let explicit = TurnToolSecurityPolicy::new(Some(vec![PathBuf::from("/explicit")]), None);
        assert_eq!(
            explicit.trusted_external_paths_override(),
            Some(&[PathBuf::from("/explicit")][..])
        );
    }
}
