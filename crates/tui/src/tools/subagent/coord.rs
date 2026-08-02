//! Narrow model-facing agent coordination tools.
//!
//! Keeps `agent` as the creation surface. These five tools wrap existing
//! SubAgentManager / mailbox / checkpoint machinery without restoring the
//! retired lifecycle theater (`agent_open` / `agent_eval` / …).

use std::sync::Arc;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use serde_json::{Value, json};

use super::{
    COMPLETED_AGENT_RETENTION, SharedSubAgentManager, SubAgentRuntime, SubAgentStatus,
    parse_agent_ref, subagent_session_projection, subagent_status_name,
    wait_for_subagents_from_input,
};
use crate::tools::registry::ToolRegistryBuilder;
use crate::tools::spec::{
    ApprovalRequirement, ToolCapability, ToolContext, ToolError, ToolResult, ToolSpec,
};

const COORD_WAIT_DEFAULT_TIMEOUT_SECS: u64 = 300;
const COORD_WAIT_MIN_TIMEOUT_SECS: u64 = 1;
const COORD_WAIT_MAX_TIMEOUT_SECS: u64 = 1800;
const COORD_WAIT_CHECK_INTERVAL: Duration = Duration::from_millis(250);
const RECENT_PROGRESS_LIMIT: usize = 8;

// ── agents/list ──────────────────────────────────────────────────────────

pub struct AgentsListTool {
    manager: SharedSubAgentManager,
}

impl AgentsListTool {
    #[must_use]
    pub fn new(manager: SharedSubAgentManager) -> Self {
        Self { manager }
    }
}

#[async_trait]
impl ToolSpec for AgentsListTool {
    fn name(&self) -> &'static str {
        "agents/list"
    }

    fn description(&self) -> &'static str {
        "List child agents: ids, parent hierarchy, state, bounded recent progress, and token budget. Read-only coordination view — does not spawn or wake workers."
    }

    fn input_schema(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "include_archived": {
                    "type": "boolean",
                    "description": "Include prior-session / archived agents. Default false."
                },
                "agent_id": {
                    "type": "string",
                    "description": "Optional single agent id or session name to inspect."
                }
            },
            "required": []
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::ReadOnly]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Auto
    }

    fn is_read_only_for(&self, _input: &Value) -> bool {
        true
    }

    fn supports_parallel_for(&self, _input: &Value) -> bool {
        true
    }

    async fn execute(&self, input: Value, _context: &ToolContext) -> Result<ToolResult, ToolError> {
        let include_archived = input
            .get("include_archived")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        let agent_ref = parse_agent_ref(&input);

        let mut manager = self.manager.write().await;
        manager.cleanup(COMPLETED_AGENT_RETENTION);
        let summaries = if let Some(agent_ref) = agent_ref {
            let summary = manager
                .coordination_summary_for(&agent_ref, RECENT_PROGRESS_LIMIT)
                .map_err(|err| ToolError::invalid_input(err.to_string()))?;
            vec![summary]
        } else {
            manager.list_coordination_summaries(include_archived, RECENT_PROGRESS_LIMIT)
        };
        drop(manager);

        let payload = json!({
            "action": "list",
            "count": summaries.len(),
            "agents": summaries,
        });
        let mut tool_result = ToolResult::json(&payload)
            .map_err(|err| ToolError::execution_failed(err.to_string()))?;
        tool_result.metadata = Some(json!({
            "action": "list",
            "count": summaries.len(),
        }));
        Ok(tool_result)
    }
}

// ── agents/message ───────────────────────────────────────────────────────

pub struct AgentsMessageTool {
    manager: SharedSubAgentManager,
}

impl AgentsMessageTool {
    #[must_use]
    pub fn new(manager: SharedSubAgentManager) -> Self {
        Self { manager }
    }
}

#[async_trait]
impl ToolSpec for AgentsMessageTool {
    fn name(&self) -> &'static str {
        "agents/message"
    }

    fn description(&self) -> &'static str {
        "Queue a parent message onto a child agent without waking it. The message stays queued until a later agents/followup delivers it to the running child. Use agents/followup directly when you want that immediate delivery."
    }

    fn input_schema(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "agent_id": {
                    "type": "string",
                    "description": "Target child agent id or session name."
                },
                "message": {
                    "type": "string",
                    "description": "Message text to queue."
                }
            },
            "required": ["agent_id", "message"]
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::RequiresApproval]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Required
    }

    async fn execute(&self, input: Value, _context: &ToolContext) -> Result<ToolResult, ToolError> {
        let agent_ref =
            parse_agent_ref(&input).ok_or_else(|| ToolError::missing_field("agent_id"))?;
        let message = input
            .get("message")
            .or_else(|| input.get("text"))
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| ToolError::missing_field("message"))?
            .to_string();

        let receipt = {
            let mut manager = self.manager.write().await;
            manager
                .queue_parent_message(&agent_ref, message, false)
                .map_err(|err| ToolError::invalid_input(err.to_string()))?
        };

        let payload = json!({
            "action": "message",
            "agent_id": receipt.agent_id,
            "queued": true,
            "woke": false,
            "queue_depth": receipt.queue_depth,
            "status": receipt.status,
            "note": "Message queued without waking the child.",
        });
        let mut tool_result = ToolResult::json(&payload)
            .map_err(|err| ToolError::execution_failed(err.to_string()))?;
        tool_result.metadata = Some(json!({
            "action": "message",
            "agent_id": receipt.agent_id,
            "woke": false,
            "queue_depth": receipt.queue_depth,
        }));
        Ok(tool_result)
    }
}

// ── agents/followup ──────────────────────────────────────────────────────

pub struct AgentsFollowupTool {
    manager: SharedSubAgentManager,
}

impl AgentsFollowupTool {
    #[must_use]
    pub fn new(manager: SharedSubAgentManager) -> Self {
        Self { manager }
    }
}

#[async_trait]
impl ToolSpec for AgentsFollowupTool {
    fn name(&self) -> &'static str {
        "agents/followup"
    }

    fn description(&self) -> &'static str {
        "Queue a message and deliver it live to a still-running child on its next step. Interrupted children are not resumed: the message stays queued and interrupted_continuable children return their continuation_handle — live in-place resume is not automated yet (re-dispatch via agent)."
    }

    fn input_schema(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "agent_id": {
                    "type": "string",
                    "description": "Target child agent id or session name."
                },
                "message": {
                    "type": "string",
                    "description": "Follow-up message text."
                }
            },
            "required": ["agent_id", "message"]
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::RequiresApproval]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Required
    }

    async fn execute(&self, input: Value, _context: &ToolContext) -> Result<ToolResult, ToolError> {
        let agent_ref =
            parse_agent_ref(&input).ok_or_else(|| ToolError::missing_field("agent_id"))?;
        let message = input
            .get("message")
            .or_else(|| input.get("text"))
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| ToolError::missing_field("message"))?
            .to_string();

        let receipt = {
            let mut manager = self.manager.write().await;
            manager
                .followup_child(&agent_ref, message)
                .map_err(|err| ToolError::invalid_input(err.to_string()))?
        };

        let payload = json!({
            "action": "followup",
            "agent_id": receipt.agent_id,
            "queued": true,
            "woke": receipt.woke,
            "queue_depth": receipt.queue_depth,
            "status": receipt.status,
            "continued_from_checkpoint": receipt.continued_from_checkpoint,
            "continuation_handle": receipt.continuation_handle,
            "note": receipt.note,
        });
        let mut tool_result = ToolResult::json(&payload)
            .map_err(|err| ToolError::execution_failed(err.to_string()))?;
        tool_result.metadata = Some(json!({
            "action": "followup",
            "agent_id": receipt.agent_id,
            "woke": receipt.woke,
            "continued_from_checkpoint": receipt.continued_from_checkpoint,
            "continuation_handle": receipt.continuation_handle,
        }));
        Ok(tool_result)
    }
}

// ── agents/interrupt ─────────────────────────────────────────────────────

pub struct AgentsInterruptTool {
    manager: SharedSubAgentManager,
    /// Optional caller identity for fail-closed self-interrupt checks.
    caller_agent_id: Option<String>,
}

impl AgentsInterruptTool {
    #[must_use]
    pub fn new(manager: SharedSubAgentManager) -> Self {
        Self {
            manager,
            caller_agent_id: None,
        }
    }

    #[must_use]
    #[allow(dead_code)] // arms self-interrupt fail-closed when child registries thread caller (P1.2)
    pub fn with_caller(mut self, caller_agent_id: impl Into<String>) -> Self {
        self.caller_agent_id = Some(caller_agent_id.into());
        self
    }
}

#[async_trait]
impl ToolSpec for AgentsInterruptTool {
    fn name(&self) -> &'static str {
        "agents/interrupt"
    }

    fn description(&self) -> &'static str {
        "Interrupt a running child agent, preserve its checkpoint, and return the prior state. Fails closed on root or self targets. Prefer this over cancel when you may resume later."
    }

    fn input_schema(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "agent_id": {
                    "type": "string",
                    "description": "Child agent id or session name to interrupt."
                },
                "reason": {
                    "type": "string",
                    "description": "Optional interrupt reason recorded on the checkpoint."
                }
            },
            "required": ["agent_id"]
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::RequiresApproval]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Required
    }

    async fn execute(&self, input: Value, context: &ToolContext) -> Result<ToolResult, ToolError> {
        let agent_ref =
            parse_agent_ref(&input).ok_or_else(|| ToolError::missing_field("agent_id"))?;
        let reason = input
            .get("reason")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .unwrap_or("interrupted by parent via agents/interrupt")
            .to_string();

        let (prior, snapshot) = {
            let mut manager = self.manager.write().await;
            manager
                .interrupt_child(&agent_ref, self.caller_agent_id.as_deref(), reason)
                .map_err(|err| ToolError::invalid_input(err.to_string()))?
        };

        let worker_record = {
            let manager = self.manager.read().await;
            manager.get_worker_record(&snapshot.agent_id)
        };
        let projection = subagent_session_projection(snapshot, false, context, worker_record).await;
        let payload = json!({
            "action": "interrupt",
            "agent_id": projection.agent_id,
            "prior_status": subagent_status_name(&prior.status),
            "prior_steps_taken": prior.steps_taken,
            "status": projection.status,
            "checkpoint_preserved": projection.checkpoint.is_some(),
            "continuable": projection.continuable,
            "projection": projection,
        });
        let mut tool_result = ToolResult::json(&payload)
            .map_err(|err| ToolError::execution_failed(err.to_string()))?;
        tool_result.metadata = Some(json!({
            "action": "interrupt",
            "agent_id": payload["agent_id"],
            "checkpoint_preserved": payload["checkpoint_preserved"],
        }));
        Ok(tool_result)
    }
}

// ── agents/wait ──────────────────────────────────────────────────────────

pub struct AgentsWaitTool {
    manager: SharedSubAgentManager,
}

impl AgentsWaitTool {
    #[must_use]
    pub fn new(manager: SharedSubAgentManager) -> Self {
        Self { manager }
    }
}

#[async_trait]
impl ToolSpec for AgentsWaitTool {
    fn name(&self) -> &'static str {
        "agents/wait"
    }

    fn description(&self) -> &'static str {
        "Block until a child shows activity, settles (completion/failure/interrupt), or the timeout elapses. Prefer one wait over polling agents/list. until=completion (default) waits for settle; until=activity returns on progress or settle."
    }

    fn input_schema(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "agent_id": {
                    "type": "string",
                    "description": "Optional specific child. When omitted, waits for the next watched child event."
                },
                "timeout_secs": {
                    "type": "integer",
                    "minimum": 1,
                    "maximum": 1800,
                    "description": "Maximum seconds to block. Default 300."
                },
                "until": {
                    "type": "string",
                    "enum": ["completion", "activity"],
                    "description": "completion (default): return when a child leaves running. activity: also return when recent progress changes."
                }
            },
            "required": []
        })
    }

    fn capabilities(&self) -> Vec<ToolCapability> {
        vec![ToolCapability::ReadOnly]
    }

    fn approval_requirement(&self) -> ApprovalRequirement {
        ApprovalRequirement::Auto
    }

    fn is_read_only_for(&self, _input: &Value) -> bool {
        true
    }

    async fn execute(&self, input: Value, context: &ToolContext) -> Result<ToolResult, ToolError> {
        let until = input
            .get("until")
            .and_then(Value::as_str)
            .unwrap_or("completion")
            .trim()
            .to_ascii_lowercase();

        if until == "completion" || until.is_empty() {
            let mut wait_input = input.clone();
            if wait_input.get("action").is_none() {
                wait_input["action"] = json!("wait");
            }
            return wait_for_subagents_from_input(&wait_input, Arc::clone(&self.manager), context)
                .await;
        }

        if until != "activity" {
            return Err(ToolError::invalid_input(format!(
                "Invalid until '{until}'. Use completion or activity."
            )));
        }

        wait_for_activity(&input, Arc::clone(&self.manager), context).await
    }
}

async fn wait_for_activity(
    input: &Value,
    manager: SharedSubAgentManager,
    context: &ToolContext,
) -> Result<ToolResult, ToolError> {
    let timeout_secs = input
        .get("timeout_secs")
        .or_else(|| input.get("timeout"))
        .and_then(Value::as_u64)
        .unwrap_or(COORD_WAIT_DEFAULT_TIMEOUT_SECS)
        .clamp(COORD_WAIT_MIN_TIMEOUT_SECS, COORD_WAIT_MAX_TIMEOUT_SECS);
    let timeout = Duration::from_secs(timeout_secs);
    let agent_ref = parse_agent_ref(input);

    let (watched, baseline): (Vec<String>, Vec<(String, u64)>) = {
        let manager = manager.read().await;
        if let Some(agent_ref) = &agent_ref {
            let snap = manager
                .get_result_by_ref(agent_ref)
                .map_err(|err| ToolError::invalid_input(err.to_string()))?;
            let fp = manager.activity_fingerprint(&snap.agent_id).unwrap_or(0);
            if snap.status != SubAgentStatus::Running {
                let payload = json!({
                    "action": "wait",
                    "until": "activity",
                    "reason": "already_settled",
                    "timed_out": false,
                    "agent_id": snap.agent_id,
                    "status": subagent_status_name(&snap.status),
                });
                let mut tool_result = ToolResult::json(&payload)
                    .map_err(|err| ToolError::execution_failed(err.to_string()))?;
                tool_result.metadata = Some(json!({ "action": "wait", "timed_out": false }));
                return Ok(tool_result);
            }
            (vec![snap.agent_id.clone()], vec![(snap.agent_id, fp)])
        } else {
            let running = manager
                .list_filtered(false)
                .into_iter()
                .filter(|s| s.status == SubAgentStatus::Running)
                .map(|s| s.agent_id)
                .collect::<Vec<_>>();
            let baseline = running
                .iter()
                .map(|id| {
                    let fp = manager.activity_fingerprint(id).unwrap_or(0);
                    (id.clone(), fp)
                })
                .collect();
            (running, baseline)
        }
    };

    if watched.is_empty() {
        let payload = json!({
            "action": "wait",
            "until": "activity",
            "note": "No running sub-agents; nothing to wait for.",
            "timed_out": false,
        });
        let mut tool_result = ToolResult::json(&payload)
            .map_err(|err| ToolError::execution_failed(err.to_string()))?;
        tool_result.metadata = Some(json!({ "action": "wait", "timed_out": false }));
        return Ok(tool_result);
    }

    let started = Instant::now();
    let cancelled = async {
        match &context.cancel_token {
            Some(token) => token.cancelled().await,
            None => std::future::pending().await,
        }
    };
    tokio::pin!(cancelled);

    loop {
        let outcome = {
            let manager = manager.read().await;
            let mut settled = Vec::new();
            let mut activity = Vec::new();
            for (id, base_fp) in &baseline {
                if let Ok(snap) = manager.get_result_by_ref(id) {
                    if snap.status != SubAgentStatus::Running {
                        settled.push(snap);
                        continue;
                    }
                    let fp = manager.activity_fingerprint(id).unwrap_or(0);
                    if fp != *base_fp {
                        activity.push(json!({
                            "agent_id": id,
                            "status": "running",
                            "activity_fingerprint": fp,
                        }));
                    }
                }
            }
            (settled, activity, manager.running_count())
        };

        if !outcome.0.is_empty() || !outcome.1.is_empty() {
            let payload = json!({
                "action": "wait",
                "until": "activity",
                "settled": outcome.0.iter().map(|s| json!({
                    "agent_id": s.agent_id,
                    "status": subagent_status_name(&s.status),
                })).collect::<Vec<_>>(),
                "activity": outcome.1,
                "running": outcome.2,
                "elapsed_ms": started.elapsed().as_millis(),
                "timed_out": false,
            });
            let mut tool_result = ToolResult::json(&payload)
                .map_err(|err| ToolError::execution_failed(err.to_string()))?;
            tool_result.metadata = Some(json!({
                "action": "wait",
                "timed_out": false,
                "settled": outcome.0.len(),
                "activity": outcome.1.len(),
            }));
            return Ok(tool_result);
        }

        if started.elapsed() >= timeout {
            let payload = json!({
                "action": "wait",
                "until": "activity",
                "settled": [],
                "activity": [],
                "running": outcome.2,
                "elapsed_ms": started.elapsed().as_millis(),
                "timed_out": true,
                "note": "Timed out before child activity or completion.",
            });
            let mut tool_result = ToolResult::json(&payload)
                .map_err(|err| ToolError::execution_failed(err.to_string()))?;
            tool_result.metadata = Some(json!({ "action": "wait", "timed_out": true }));
            return Ok(tool_result);
        }

        tokio::select! {
            biased;
            () = &mut cancelled => {
                return Err(ToolError::execution_failed(
                    "Wait interrupted by user cancellation before child activity.".to_string(),
                ));
            }
            () = tokio::time::sleep(COORD_WAIT_CHECK_INTERVAL) => {}
        }
    }
}

/// Register the five coordination tools alongside `agent`.
pub fn register_coordination_tools(
    builder: ToolRegistryBuilder,
    manager: SharedSubAgentManager,
    runtime: SubAgentRuntime,
) -> ToolRegistryBuilder {
    // `runtime.parent_agent_id` is the identity of the agent this registry is
    // being built FOR: `runtime_for_nested_agent_tools` stamps the child's own
    // id there before `new_with_owner` registers tools, so anything that agent
    // spawns records it as parent. Threading it as the interrupt caller makes
    // the self-interrupt fail-closed guard live in production instead of only
    // in tests (TUI-DOG-017). `None` means the root engine registry; the root
    // is separately protected by the literal-root check in `interrupt_child`.
    let interrupt = match runtime.parent_agent_id.as_deref() {
        Some(caller) => AgentsInterruptTool::new(Arc::clone(&manager)).with_caller(caller),
        None => AgentsInterruptTool::new(Arc::clone(&manager)),
    };
    builder
        .with_tool(Arc::new(AgentsListTool::new(Arc::clone(&manager))))
        .with_tool(Arc::new(AgentsMessageTool::new(Arc::clone(&manager))))
        .with_tool(Arc::new(AgentsFollowupTool::new(Arc::clone(&manager))))
        .with_tool(Arc::new(interrupt))
        .with_tool(Arc::new(AgentsWaitTool::new(manager)))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tools::spec::ToolContext;
    use tempfile::tempdir;

    async fn manager_with_running_child(
        workspace: &std::path::Path,
    ) -> (SharedSubAgentManager, String) {
        let manager = Arc::new(tokio::sync::RwLock::new(
            super::super::SubAgentManager::new(workspace.to_path_buf(), 4),
        ));
        let agent_id = {
            let mut guard = manager.write().await;
            guard.insert_test_running_agent("coord_child", workspace)
        };
        (manager, agent_id)
    }

    #[tokio::test]
    async fn message_queues_without_waking() {
        let tmp = tempdir().unwrap();
        let (manager, agent_id) = manager_with_running_child(tmp.path()).await;
        let tool = AgentsMessageTool::new(Arc::clone(&manager));
        let result = tool
            .execute(
                json!({ "agent_id": agent_id, "message": "hold this" }),
                &ToolContext::new(tmp.path()),
            )
            .await
            .expect("message ok");
        let body: Value = serde_json::from_str(&result.content).unwrap();
        assert_eq!(body["woke"], json!(false));
        assert_eq!(body["queued"], json!(true));
        assert_eq!(body["queue_depth"], json!(1));

        let guard = manager.read().await;
        let depth = guard.queued_mail_depth(&agent_id).unwrap();
        assert_eq!(depth, 1);
        assert!(!guard.child_was_woken(&agent_id));
    }

    #[tokio::test]
    async fn interrupt_fails_closed_on_self() {
        let tmp = tempdir().unwrap();
        let (manager, agent_id) = manager_with_running_child(tmp.path()).await;
        let tool = AgentsInterruptTool::new(Arc::clone(&manager)).with_caller(agent_id.clone());
        let err = tool
            .execute(
                json!({ "agent_id": agent_id }),
                &ToolContext::new(tmp.path()),
            )
            .await
            .expect_err("self interrupt must fail");
        let msg = err.to_string().to_ascii_lowercase();
        assert!(
            msg.contains("self") || msg.contains("own"),
            "unexpected error: {err}"
        );
    }

    #[tokio::test]
    async fn interrupt_fails_closed_on_missing_target() {
        let tmp = tempdir().unwrap();
        let manager = Arc::new(tokio::sync::RwLock::new(
            super::super::SubAgentManager::new(tmp.path().to_path_buf(), 2),
        ));
        let tool = AgentsInterruptTool::new(manager);
        let err = tool
            .execute(
                json!({ "agent_id": "agent_missing" }),
                &ToolContext::new(tmp.path()),
            )
            .await
            .expect_err("missing target");
        assert!(err.to_string().contains("not found") || err.to_string().contains("Agent"));
    }

    #[tokio::test]
    async fn wait_times_out_when_child_stays_running() {
        let tmp = tempdir().unwrap();
        let (manager, agent_id) = manager_with_running_child(tmp.path()).await;
        let tool = AgentsWaitTool::new(manager);
        let result = tool
            .execute(
                json!({
                    "agent_id": agent_id,
                    "timeout_secs": 1,
                    "until": "activity"
                }),
                &ToolContext::new(tmp.path()),
            )
            .await
            .expect("wait returns");
        let body: Value = serde_json::from_str(&result.content).unwrap();
        assert_eq!(body["timed_out"], json!(true));
    }

    #[tokio::test]
    async fn list_resolves_target_and_reports_queue() {
        let tmp = tempdir().unwrap();
        let (manager, agent_id) = manager_with_running_child(tmp.path()).await;
        {
            let mut guard = manager.write().await;
            guard
                .queue_parent_message(&agent_id, "note".into(), false)
                .unwrap();
        }
        let tool = AgentsListTool::new(manager);
        let result = tool
            .execute(
                json!({ "agent_id": agent_id }),
                &ToolContext::new(tmp.path()),
            )
            .await
            .expect("list ok");
        let body: Value = serde_json::from_str(&result.content).unwrap();
        assert_eq!(body["count"], json!(1));
        assert_eq!(body["agents"][0]["agent_id"], json!(agent_id));
        assert!(body["agents"][0]["queued_mail"].as_u64().unwrap_or(0) >= 1);
    }

    #[tokio::test]
    async fn followup_interrupted_continuable_queues_honestly_without_auto_resume() {
        let tmp = tempdir().unwrap();
        let manager = Arc::new(tokio::sync::RwLock::new(
            super::super::SubAgentManager::new(tmp.path().to_path_buf(), 4),
        ));
        let (agent_id, handle) = {
            let mut guard = manager.write().await;
            guard.insert_test_interrupted_continuable_agent(
                "paused_child",
                tmp.path(),
                vec![crate::models::Message {
                    role: "user".to_string(),
                    content: vec![crate::models::ContentBlock::Text {
                        text: "prior work".to_string(),
                        cache_control: None,
                    }],
                }],
            )
        };
        let tool = AgentsFollowupTool::new(Arc::clone(&manager));
        let result = tool
            .execute(
                json!({ "agent_id": agent_id, "message": "please continue" }),
                &ToolContext::new(tmp.path()),
            )
            .await
            .expect("followup ok");
        let body: Value = serde_json::from_str(&result.content).unwrap();
        assert_eq!(body["queued"], json!(true));
        assert_eq!(body["woke"], json!(false));
        assert_eq!(body["continued_from_checkpoint"], json!(false));
        assert_eq!(body["continuation_handle"], json!(handle));
        let note = body["note"].as_str().unwrap_or_default();
        assert!(
            note.contains("not automated") && note.contains(&handle),
            "note must fail honestly with the continuation handle: {note}"
        );

        let guard = manager.read().await;
        assert_eq!(guard.queued_mail_depth(&agent_id).unwrap(), 1);
        assert!(!guard.child_was_woken(&agent_id));
    }
}
