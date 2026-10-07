//! Minimal, revisioned B capability snapshot.
//!
//! A snapshot is a display/admission hint, never an authorization bypass. The
//! B task owner must revalidate the exact workspace and tool policy again when
//! admitting work. No source path, Skill body, environment variable, credential,
//! or package content crosses this boundary.

use std::collections::{BTreeMap, HashSet};

use parking_lot::{Condvar, Mutex};
use serde::Serialize;
use serde_json::Value;

use super::controlled::ControlledWorkspaceInfo;
use super::protocol::LeaseContext;

const SNAPSHOT_TTL_MS: u64 = 30_000;
const MAX_DESCRIPTORS: usize = 256;
const MAX_ID_CHARS: usize = 128;
// Frozen capability_snapshot wire grammar counts Unicode scalars, not bytes.
pub(crate) const MAX_DISPLAY_CHARS: usize = 128;

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct CapabilityError {
    pub(crate) code: &'static str,
}

impl CapabilityError {
    const fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl std::fmt::Display for CapabilityError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for CapabilityError {}

type Result<T> = std::result::Result<T, CapabilityError>;

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct CapabilityDescriptor {
    pub(crate) id: String,
    pub(crate) display_name: String,
}

#[derive(Debug, Clone, Default)]
pub(crate) struct CapabilitySource {
    pub(crate) code_agent_ready: bool,
    pub(crate) tools: Vec<CapabilityDescriptor>,
    pub(crate) plain_skills: Vec<CapabilityDescriptor>,
    pub(crate) code_skills: Vec<CapabilityDescriptor>,
    pub(crate) workspaces: Vec<ControlledWorkspaceInfo>,
}

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct RemoteCapabilityTool {
    tool_id: String,
    display_name: String,
}

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct RemoteCapabilitySkill {
    skill_id: String,
    display_name: String,
    scopes: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct RemoteCapabilityWorkspace {
    workspace_id: String,
    display_name: String,
    code_allowed: bool,
}

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct CapabilitySnapshot {
    connection_id: String,
    controller_lease_id: String,
    lease_epoch: String,
    relay_epoch: String,
    remote_control_generation: String,
    capability_revision: String,
    expires_at_ms: String,
    modes: Vec<String>,
    tools: Vec<RemoteCapabilityTool>,
    skills: Vec<RemoteCapabilitySkill>,
    workspaces: Vec<RemoteCapabilityWorkspace>,
}

#[derive(Default)]
pub(crate) struct CapabilitySnapshotManager {
    current: Mutex<Option<CapabilitySnapshot>>,
    revision: Mutex<u64>,
    admission_gate: CapabilityAdmissionGate,
}

#[derive(Default)]
struct CapabilityAdmissionGate {
    state: Mutex<CapabilityAdmissionGateState>,
    changed: Condvar,
}

#[derive(Default)]
struct CapabilityAdmissionGateState {
    active_admissions: usize,
    refreshing: bool,
    failed: bool,
}

/// A short-lived consistency token. It does not hold a mutex while the ledger
/// persists its record, but it makes snapshot replacement linearize after the
/// admission finishes. This closes the validate/refresh/admit TOCTOU without
/// carrying a capability mutex across filesystem I/O.
pub(crate) struct CapabilityAdmissionGuard<'a> {
    gate: &'a CapabilityAdmissionGate,
}

impl std::fmt::Debug for CapabilityAdmissionGuard<'_> {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("CapabilityAdmissionGuard(..)")
    }
}

struct CapabilityRefreshGuard<'a> {
    gate: &'a CapabilityAdmissionGate,
}

impl CapabilityAdmissionGate {
    fn begin_admission(&self) -> Result<CapabilityAdmissionGuard<'_>> {
        let mut state = self.state.lock();
        while state.refreshing && !state.failed {
            self.changed.wait(&mut state);
        }
        if state.failed {
            return Err(CapabilityError::new("capability_admission_corrupt"));
        }
        state.active_admissions = state
            .active_admissions
            .checked_add(1)
            .ok_or_else(|| CapabilityError::new("capability_admission_capacity"))?;
        Ok(CapabilityAdmissionGuard { gate: self })
    }

    fn begin_refresh(&self) -> Result<CapabilityRefreshGuard<'_>> {
        let mut state = self.state.lock();
        while (state.refreshing || state.active_admissions != 0) && !state.failed {
            self.changed.wait(&mut state);
        }
        if state.failed {
            return Err(CapabilityError::new("capability_admission_corrupt"));
        }
        state.refreshing = true;
        Ok(CapabilityRefreshGuard { gate: self })
    }
}

impl Drop for CapabilityAdmissionGuard<'_> {
    fn drop(&mut self) {
        let mut state = self.gate.state.lock();
        let Some(remaining) = state.active_admissions.checked_sub(1) else {
            // A broken ownership invariant cannot reopen the admission gate.
            // Wake waiters so they fail closed instead of waiting indefinitely.
            state.failed = true;
            self.gate.changed.notify_all();
            log::error!("capability admission guard underflow; gate failed closed");
            return;
        };
        state.active_admissions = remaining;
        if state.active_admissions == 0 {
            self.gate.changed.notify_all();
        }
    }
}

impl Drop for CapabilityRefreshGuard<'_> {
    fn drop(&mut self) {
        let mut state = self.gate.state.lock();
        state.refreshing = false;
        self.gate.changed.notify_all();
    }
}

impl CapabilitySnapshotManager {
    pub(crate) fn build(
        &self,
        context: LeaseContext,
        source: CapabilitySource,
        now_b: u64,
    ) -> Result<CapabilitySnapshot> {
        validate_descriptors("tool", &source.tools)?;
        validate_descriptors("skill", &source.plain_skills)?;
        validate_descriptors("skill", &source.code_skills)?;
        if source.workspaces.len() > MAX_DESCRIPTORS {
            return Err(CapabilityError::new("capability_workspace_capacity"));
        }
        let mut workspace_ids = HashSet::new();
        for workspace in &source.workspaces {
            validate_id("workspace_id", &workspace.workspace_id)?;
            validate_display(&workspace.display_name)?;
            if !workspace_ids.insert(workspace.workspace_id.clone()) {
                return Err(CapabilityError::new("capability_workspace_duplicate"));
            }
        }

        let code_workspaces: Vec<_> = source
            .workspaces
            .iter()
            .filter(|workspace| workspace.code_allowed)
            .cloned()
            .collect();
        let code_mode_allowed = source.code_agent_ready && !code_workspaces.is_empty();
        let mut skills = BTreeMap::<String, (String, Vec<String>)>::new();
        for skill in source.plain_skills {
            skills.insert(skill.id, (skill.display_name, vec!["chat".to_owned()]));
        }
        if code_mode_allowed {
            for skill in source.code_skills {
                let entry = skills
                    .entry(skill.id)
                    .or_insert_with(|| (skill.display_name.clone(), Vec::new()));
                if entry.0 != skill.display_name {
                    return Err(CapabilityError::new("capability_skill_display_conflict"));
                }
                entry.1.push("code".to_owned());
            }
        }
        if skills.len() > MAX_DESCRIPTORS {
            return Err(CapabilityError::new("capability_descriptor_capacity"));
        }

        // Snapshot publication is serialized with final ledger admission by a
        // consistency token rather than a mutex held across ledger persistence.
        let _refresh = self.admission_gate.begin_refresh()?;
        let mut revision = self.revision.lock();
        *revision = revision
            .checked_add(1)
            .ok_or_else(|| CapabilityError::new("capability_revision_exhausted"))?;
        let expires_at_ms = now_b
            .checked_add(SNAPSHOT_TTL_MS)
            .ok_or_else(|| CapabilityError::new("capability_expiry_overflow"))?;
        let snapshot = CapabilitySnapshot {
            connection_id: context.connection_id,
            controller_lease_id: context.controller_lease_id,
            lease_epoch: context.lease_epoch,
            relay_epoch: context.relay_epoch,
            remote_control_generation: context.remote_control_generation,
            capability_revision: revision.to_string(),
            expires_at_ms: expires_at_ms.to_string(),
            modes: if code_mode_allowed {
                vec!["chat".into(), "code".into()]
            } else {
                vec!["chat".into()]
            },
            tools: source
                .tools
                .into_iter()
                .map(|tool| RemoteCapabilityTool {
                    tool_id: tool.id,
                    display_name: tool.display_name,
                })
                .collect(),
            skills: skills
                .into_iter()
                .map(|(skill_id, (display_name, scopes))| RemoteCapabilitySkill {
                    skill_id,
                    display_name,
                    scopes,
                })
                .collect(),
            workspaces: code_workspaces
                .into_iter()
                .map(|workspace| RemoteCapabilityWorkspace {
                    workspace_id: workspace.workspace_id,
                    display_name: workspace.display_name,
                    code_allowed: workspace.code_allowed,
                })
                .collect(),
        };
        *self.current.lock() = Some(snapshot.clone());
        Ok(snapshot)
    }

    pub(crate) fn validate(
        &self,
        revision: &str,
        context: &LeaseContext,
        now_b: u64,
    ) -> Result<CapabilitySnapshot> {
        let snapshot = self.current_for_context(context, now_b)?;
        if snapshot.capability_revision != revision {
            return Err(CapabilityError::new("capability_snapshot_stale"));
        }
        Ok(snapshot)
    }

    /// Return the current unexpired snapshot for the exact lease. This is used
    /// when A has not observed a revision yet (known revision 0), for example
    /// after a response was lost. Reusing it avoids repeating expensive ACP and
    /// marketplace discovery for duplicate requests.
    pub(crate) fn current_for_context(
        &self,
        context: &LeaseContext,
        now_b: u64,
    ) -> Result<CapabilitySnapshot> {
        let snapshot = self
            .current
            .lock()
            .clone()
            .ok_or_else(|| CapabilityError::new("capability_snapshot_missing"))?;
        if snapshot.connection_id != context.connection_id
            || snapshot.controller_lease_id != context.controller_lease_id
            || snapshot.lease_epoch != context.lease_epoch
            || snapshot.relay_epoch != context.relay_epoch
            || snapshot.remote_control_generation != context.remote_control_generation
        {
            return Err(CapabilityError::new("capability_snapshot_stale"));
        }
        if now_b >= snapshot.expires_at_ms.parse::<u64>().unwrap_or(0) {
            return Err(CapabilityError::new("capability_snapshot_expired"));
        }
        Ok(snapshot)
    }

    /// 冻结 wire 形状的 `capability_snapshot`（B → A）。字段名与 PROTOCOL.md 的
    /// exact-object schema 一致，只含 opaque id、显示名、模式、revision 与有效期；
    /// 调用方（B 端派发层）负责再用运行时校验器自检。
    pub(crate) fn wire_capability_snapshot(
        &self,
        snapshot: &CapabilitySnapshot,
        request_id: &str,
    ) -> Value {
        serde_json::json!({
            "v": 1,
            "type": "capability_snapshot",
            "connection_id": snapshot.connection_id,
            "controller_lease_id": snapshot.controller_lease_id,
            "lease_epoch": snapshot.lease_epoch,
            "relay_epoch": snapshot.relay_epoch,
            "remote_control_generation": snapshot.remote_control_generation,
            "request_id": request_id,
            "capability_revision": snapshot.capability_revision,
            "expires_at_ms": snapshot.expires_at_ms,
            "modes": snapshot.modes,
            "tools": snapshot
                .tools
                .iter()
                .map(|tool| serde_json::json!({
                    "tool_id": tool.tool_id,
                    "display_name": tool.display_name,
                }))
                .collect::<Vec<_>>(),
            "skills": snapshot
                .skills
                .iter()
                .map(|skill| serde_json::json!({
                    "skill_id": skill.skill_id,
                    "display_name": skill.display_name,
                    "scopes": skill.scopes,
                }))
                .collect::<Vec<_>>(),
            "workspaces": snapshot
                .workspaces
                .iter()
                .map(|workspace| serde_json::json!({
                    "workspace_id": workspace.workspace_id,
                    "display_name": workspace.display_name,
                    "code_allowed": workspace.code_allowed,
                }))
                .collect::<Vec<_>>(),
        })
    }

    /// Resolve only the opaque workspace ID. Task admission must still ask the
    /// B workspace owner to recheck the live path identity and authorization.
    pub(crate) fn workspace_for_task(
        &self,
        revision: &str,
        context: &LeaseContext,
        workspace_id: &str,
        now_b: u64,
    ) -> Result<String> {
        let snapshot = self.validate(revision, context, now_b)?;
        snapshot
            .workspaces
            .iter()
            .find(|workspace| workspace.workspace_id == workspace_id)
            .map(|workspace| workspace.workspace_id.clone())
            .ok_or_else(|| CapabilityError::new("capability_workspace_not_authorized"))
    }

    /// Validate a B-local selection against the exact unexpired snapshot and
    /// the lane that will execute it. The runtime still re-resolves every
    /// selected skill from the live marketplace before reading its body.
    pub(crate) fn validate_skills_for_mode(
        &self,
        context: &LeaseContext,
        mode: &str,
        skill_ids: &[String],
        now_b: u64,
    ) -> Result<()> {
        if !matches!(mode, "chat" | "code") || skill_ids.len() > 32 {
            return Err(CapabilityError::new("task_skill_invalid"));
        }
        let snapshot = self.current_for_context(context, now_b)?;
        let mut seen = HashSet::new();
        if skill_ids.iter().any(|skill_id| {
            !seen.insert(skill_id.as_str())
                || !snapshot.skills.iter().any(|skill| {
                    skill.skill_id == *skill_id && skill.scopes.iter().any(|scope| scope == mode)
                })
        }) {
            return Err(CapabilityError::new("task_skill_invalid"));
        }
        Ok(())
    }

    /// Validate every capability-dependent task field against one exact,
    /// unexpired snapshot. The returned token must be kept alive until the
    /// ledger admission call returns; while it is alive a policy refresh may
    /// be prepared, but cannot publish a replacement snapshot.
    pub(crate) fn pin_task_admission(
        &self,
        revision: &str,
        context: &LeaseContext,
        mode: &str,
        skill_ids: &[String],
        workspace_id: Option<&str>,
        now_b: u64,
    ) -> Result<CapabilityAdmissionGuard<'_>> {
        let guard = self.admission_gate.begin_admission()?;
        self.validate_task_selection(revision, context, mode, skill_ids, workspace_id, now_b)?;
        Ok(guard)
    }

    /// Read-only preflight used before mapping/preparation. Final admission
    /// must still use `pin_task_admission` because this call intentionally does
    /// not keep a consistency token alive.
    pub(crate) fn validate_task_selection(
        &self,
        revision: &str,
        context: &LeaseContext,
        mode: &str,
        skill_ids: &[String],
        workspace_id: Option<&str>,
        now_b: u64,
    ) -> Result<()> {
        if !matches!(mode, "chat" | "code") || skill_ids.len() > 32 {
            return Err(CapabilityError::new("task_skill_invalid"));
        }
        let snapshot = self.validate(revision, context, now_b)?;
        if !snapshot.modes.iter().any(|available| available == mode) {
            return Err(CapabilityError::new("task_mode_unsupported"));
        }
        let mut seen = HashSet::new();
        if skill_ids.iter().any(|skill_id| {
            !seen.insert(skill_id.as_str())
                || !snapshot.skills.iter().any(|skill| {
                    skill.skill_id == *skill_id && skill.scopes.iter().any(|scope| scope == mode)
                })
        }) {
            return Err(CapabilityError::new("task_skill_invalid"));
        }
        match (mode, workspace_id) {
            ("chat", None) => Ok(()),
            ("chat", Some(_)) => Err(CapabilityError::new("task_workspace_unexpected")),
            ("code", Some(workspace_id))
                if snapshot.workspaces.iter().any(|workspace| {
                    workspace.code_allowed && workspace.workspace_id == workspace_id
                }) =>
            {
                Ok(())
            }
            ("code", Some(_)) => Err(CapabilityError::new("capability_workspace_not_authorized")),
            ("code", None) => Err(CapabilityError::new("task_workspace_required")),
            _ => Err(CapabilityError::new("task_mode_unsupported")),
        }
    }

    /// Convert B's monotonic expiry into an absolute UI deadline without
    /// changing the wire snapshot or minting another capability revision.
    pub(crate) fn controlled_ui_projection(
        &self,
        snapshot: &CapabilitySnapshot,
        now_b: u64,
        now_wall_ms: u64,
    ) -> Result<Value> {
        let deadline_b = snapshot
            .expires_at_ms
            .parse::<u64>()
            .map_err(|_| CapabilityError::new("capability_snapshot_invalid"))?;
        let remaining = deadline_b.saturating_sub(now_b);
        if remaining == 0 {
            return Err(CapabilityError::new("capability_snapshot_expired"));
        }
        let mut projection = serde_json::to_value(snapshot)
            .map_err(|_| CapabilityError::new("capability_snapshot_invalid"))?;
        projection["expiresAtMs"] = Value::String(
            now_wall_ms
                .checked_add(remaining)
                .ok_or_else(|| CapabilityError::new("capability_expiry_overflow"))?
                .to_string(),
        );
        Ok(projection)
    }
}

fn validate_descriptors(kind: &'static str, values: &[CapabilityDescriptor]) -> Result<()> {
    if values.len() > MAX_DESCRIPTORS {
        return Err(CapabilityError::new("capability_descriptor_capacity"));
    }
    let mut ids = HashSet::new();
    for descriptor in values {
        validate_id(kind, &descriptor.id)?;
        validate_display(&descriptor.display_name)?;
        if !ids.insert(descriptor.id.clone()) {
            return Err(CapabilityError::new("capability_descriptor_duplicate"));
        }
    }
    Ok(())
}

fn validate_id(field: &'static str, value: &str) -> Result<()> {
    if value.is_empty()
        || value.len() > MAX_ID_CHARS
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err(CapabilityError::new("capability_id_invalid"));
    }
    let _ = field;
    Ok(())
}

fn validate_display(value: &str) -> Result<()> {
    let trimmed = value.trim();
    if trimmed.is_empty() || trimmed.chars().count() > MAX_DISPLAY_CHARS {
        return Err(CapabilityError::new("capability_display_invalid"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    #[test]
    fn admission_underflow_poison_rejects_both_admission_and_refresh() {
        let gate = super::CapabilityAdmissionGate::default();
        // An invalid guard models a broken ownership invariant, not an input
        // reachable through the public pin API.
        drop(super::CapabilityAdmissionGuard { gate: &gate });
        assert!(gate.state.lock().failed);
        assert_eq!(
            gate.begin_admission().unwrap_err().code,
            "capability_admission_corrupt"
        );
        assert_eq!(
            gate.begin_refresh().err().unwrap().code,
            "capability_admission_corrupt"
        );
        assert!(!gate.state.lock().refreshing);
    }

    #[test]
    fn balanced_admission_release_preserves_refresh_and_never_poison_gates() {
        let gate = super::CapabilityAdmissionGate::default();
        let first = gate.begin_admission().unwrap();
        let second = gate.begin_admission().unwrap();
        assert_eq!(gate.state.lock().active_admissions, 2);
        drop(first);
        assert_eq!(gate.state.lock().active_admissions, 1);
        drop(second);
        assert_eq!(gate.state.lock().active_admissions, 0);
        let refresh = gate.begin_refresh().unwrap();
        assert!(gate.state.lock().refreshing);
        drop(refresh);
        assert!(!gate.state.lock().failed);
        drop(gate.begin_admission().unwrap());
    }
    use super::*;

    fn context(generation: &str) -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: generation.into(),
        }
    }

    fn workspace(id: &str, code_allowed: bool) -> ControlledWorkspaceInfo {
        ControlledWorkspaceInfo {
            workspace_id: id.into(),
            display_name: "Projects".into(),
            code_allowed,
        }
    }

    fn fixture_source() -> CapabilitySource {
        CapabilitySource {
            code_agent_ready: true,
            tools: vec![CapabilityDescriptor {
                id: "tool-browser".into(),
                display_name: "Browser".into(),
            }],
            plain_skills: vec![CapabilityDescriptor {
                id: "skill-plain".into(),
                display_name: "Plain Skill".into(),
            }],
            code_skills: vec![CapabilityDescriptor {
                id: "skill-code".into(),
                display_name: "Code Skill".into(),
            }],
            workspaces: vec![workspace("workspace-0001", true)],
        }
    }

    #[test]
    fn snapshot_hides_code_mode_and_workspace_without_preauthorization() {
        let manager = CapabilitySnapshotManager::default();
        let mut source = fixture_source();
        source.code_agent_ready = false;
        source.workspaces = vec![workspace("workspace-0001", false)];
        let snapshot = manager
            .build(context("generation-0001"), source, 1_000)
            .unwrap();
        assert_eq!(snapshot.modes, vec!["chat".to_owned()]);
        assert!(snapshot.workspaces.is_empty());
        assert_eq!(snapshot.skills.len(), 1);
        assert_eq!(snapshot.expires_at_ms, "31000");
        let encoded = serde_json::to_string(&snapshot).unwrap();
        assert!(!encoded.contains(r"D:\\"));
        assert!(!encoded.contains("api_key"));
        assert!(!encoded.contains("secret"));
    }

    #[test]
    fn complete_snapshot_is_bounded_revisioned_and_context_bound() {
        let manager = CapabilitySnapshotManager::default();
        let snapshot = manager
            .build(context("generation-0001"), fixture_source(), 1_000)
            .unwrap();
        assert_eq!(snapshot.capability_revision, "1");
        assert_eq!(snapshot.modes, vec!["chat".to_owned(), "code".to_owned()]);
        assert_eq!(snapshot.tools.len(), 1);
        assert_eq!(snapshot.skills.len(), 2);
        assert_eq!(snapshot.skills[0].scopes, vec!["code".to_owned()]);
        assert_eq!(snapshot.skills[1].scopes, vec!["chat".to_owned()]);
        assert_eq!(snapshot.workspaces.len(), 1);
        assert_eq!(
            manager
                .validate("1", &context("generation-0001"), 2_000)
                .unwrap(),
            snapshot
        );
        assert_eq!(
            manager
                .current_for_context(&context("generation-0001"), 2_000)
                .unwrap(),
            snapshot
        );
        assert_eq!(
            manager
                .validate("1", &context("generation-other"), 2_000)
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        assert_eq!(
            manager
                .validate("1", &context("generation-0001"), 31_000)
                .unwrap_err()
                .code,
            "capability_snapshot_expired"
        );

        let refreshed = manager
            .build(context("generation-0001"), fixture_source(), 5_000)
            .unwrap();
        assert_eq!(refreshed.capability_revision, "2");
        assert_eq!(
            manager
                .validate("1", &context("generation-0001"), 6_000)
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
    }

    #[test]
    fn display_boundary_matches_the_frozen_wire_validator() {
        use super::super::protocol::{
            RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
        };
        for target in ["tool", "plain_skill", "code_skill", "workspace"] {
            for length in [128, 129] {
                let manager = CapabilitySnapshotManager::default();
                let mut source = fixture_source();
                let display = "界".repeat(length);
                match target {
                    "tool" => source.tools[0].display_name = display,
                    "plain_skill" => source.plain_skills[0].display_name = display,
                    "code_skill" => source.code_skills[0].display_name = display,
                    _ => source.workspaces[0].display_name = display,
                }
                let result = manager.build(context("generation-0001"), source, 1_000);
                if length == 129 {
                    assert_eq!(result.unwrap_err().code, "capability_display_invalid");
                    assert!(
                        manager
                            .current_for_context(&context("generation-0001"), 1_001)
                            .is_err()
                    );
                } else {
                    let snapshot = result.unwrap();
                    let mut wire = manager.wire_capability_snapshot(&snapshot, "request-0001");
                    wire["session_id"] = serde_json::json!("remote-ref-1");
                    wire["controlled_session_id"] = serde_json::json!("b-session-1");
                    validate_controller_runtime_payload_json(
                        &wire.to_string(),
                        SenderRole::ControlledB,
                        Some(&RuntimeValidationContext {
                            lease: Some(context("generation-0001")),
                            negotiated_capabilities: vec![
                                "encrypted_frames_v1".into(),
                                "task_v1".into(),
                            ],
                        }),
                    )
                    .unwrap();
                }
            }
        }
    }

    #[test]
    fn one_skill_can_preserve_both_execution_scopes() {
        let manager = CapabilitySnapshotManager::default();
        let mut source = fixture_source();
        source.plain_skills = vec![CapabilityDescriptor {
            id: "skill-shared".into(),
            display_name: "Shared Skill".into(),
        }];
        source.code_skills = source.plain_skills.clone();
        let snapshot = manager
            .build(context("generation-0001"), source, 1_000)
            .unwrap();
        assert_eq!(snapshot.skills.len(), 1);
        assert_eq!(snapshot.skills[0].skill_id, "skill-shared");
        assert_eq!(snapshot.skills[0].scopes, vec!["chat", "code"]);
        manager
            .validate_skills_for_mode(
                &context("generation-0001"),
                "chat",
                &["skill-shared".into()],
                2_000,
            )
            .unwrap();
        manager
            .validate_skills_for_mode(
                &context("generation-0001"),
                "code",
                &["skill-shared".into()],
                2_000,
            )
            .unwrap();
    }

    #[test]
    fn final_chat_admission_rechecks_expiry_revision_mode_and_skill_scope() {
        let manager = CapabilitySnapshotManager::default();
        manager
            .build(context("generation-0001"), fixture_source(), 1_000)
            .unwrap();
        manager
            .pin_task_admission(
                "1",
                &context("generation-0001"),
                "chat",
                &["skill-plain".into()],
                None,
                30_999,
            )
            .unwrap();
        assert_eq!(
            manager
                .pin_task_admission(
                    "1",
                    &context("generation-0001"),
                    "chat",
                    &["skill-code".into()],
                    None,
                    2_000,
                )
                .unwrap_err()
                .code,
            "task_skill_invalid"
        );
        assert_eq!(
            manager
                .pin_task_admission("1", &context("generation-other"), "chat", &[], None, 2_000,)
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        // Models an A reserve followed by an attachment upload that outlives
        // the 30 second snapshot: B rejects at commit/admission, not dispatch.
        assert_eq!(
            manager
                .pin_task_admission("1", &context("generation-0001"), "chat", &[], None, 31_000,)
                .unwrap_err()
                .code,
            "capability_snapshot_expired"
        );
    }

    #[test]
    fn policy_refresh_invalidates_old_revision_and_removed_chat_skill() {
        let manager = CapabilitySnapshotManager::default();
        manager
            .build(context("generation-0001"), fixture_source(), 1_000)
            .unwrap();
        let mut refreshed = fixture_source();
        refreshed.plain_skills = vec![CapabilityDescriptor {
            id: "skill-new".into(),
            display_name: "New Skill".into(),
        }];
        manager
            .build(context("generation-0001"), refreshed, 2_000)
            .unwrap();
        assert_eq!(
            manager
                .pin_task_admission("1", &context("generation-0001"), "chat", &[], None, 2_001,)
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        assert_eq!(
            manager
                .pin_task_admission(
                    "2",
                    &context("generation-0001"),
                    "chat",
                    &["skill-plain".into()],
                    None,
                    2_001,
                )
                .unwrap_err()
                .code,
            "task_skill_invalid"
        );
    }

    #[test]
    fn code_admission_keeps_workspace_and_scope_checks() {
        let manager = CapabilitySnapshotManager::default();
        manager
            .build(context("generation-0001"), fixture_source(), 1_000)
            .unwrap();
        manager
            .pin_task_admission(
                "1",
                &context("generation-0001"),
                "code",
                &["skill-code".into()],
                Some("workspace-0001"),
                2_000,
            )
            .unwrap();
        assert_eq!(
            manager
                .pin_task_admission(
                    "1",
                    &context("generation-0001"),
                    "code",
                    &["skill-plain".into()],
                    Some("workspace-0001"),
                    2_000,
                )
                .unwrap_err()
                .code,
            "task_skill_invalid"
        );
        assert_eq!(
            manager
                .pin_task_admission(
                    "1",
                    &context("generation-0001"),
                    "code",
                    &["skill-code".into()],
                    Some("workspace-other"),
                    2_000,
                )
                .unwrap_err()
                .code,
            "capability_workspace_not_authorized"
        );

        let unavailable = CapabilitySnapshotManager::default();
        let mut chat_only = fixture_source();
        chat_only.code_agent_ready = false;
        unavailable
            .build(context("generation-0001"), chat_only, 1_000)
            .unwrap();
        assert_eq!(
            unavailable
                .pin_task_admission(
                    "1",
                    &context("generation-0001"),
                    "code",
                    &[],
                    Some("workspace-0001"),
                    2_000,
                )
                .unwrap_err()
                .code,
            "task_mode_unsupported"
        );
    }

    #[test]
    fn concurrent_refresh_cannot_replace_snapshot_during_admission() {
        use std::sync::{Arc, mpsc};
        use std::time::Duration;

        let manager = Arc::new(CapabilitySnapshotManager::default());
        manager
            .build(context("generation-0001"), fixture_source(), 1_000)
            .unwrap();
        let admission = manager
            .pin_task_admission(
                "1",
                &context("generation-0001"),
                "chat",
                &["skill-plain".into()],
                None,
                2_000,
            )
            .unwrap();
        let refreshing = Arc::clone(&manager);
        let (started_tx, started_rx) = mpsc::channel();
        let (done_tx, done_rx) = mpsc::channel();
        let worker = std::thread::spawn(move || {
            started_tx.send(()).unwrap();
            let refreshed = refreshing
                .build(context("generation-0001"), fixture_source(), 2_100)
                .unwrap();
            done_tx.send(refreshed.capability_revision).unwrap();
        });
        started_rx.recv().unwrap();
        assert!(done_rx.recv_timeout(Duration::from_millis(50)).is_err());
        assert_eq!(
            manager
                .current_for_context(&context("generation-0001"), 2_100)
                .unwrap()
                .capability_revision,
            "1"
        );
        drop(admission);
        assert_eq!(done_rx.recv_timeout(Duration::from_secs(1)).unwrap(), "2");
        worker.join().unwrap();
    }

    #[test]
    fn workspace_lookup_is_only_a_hint_that_requires_live_recheck() {
        let manager = CapabilitySnapshotManager::default();
        manager
            .build(context("generation-0001"), fixture_source(), 1_000)
            .unwrap();
        assert_eq!(
            manager
                .workspace_for_task("1", &context("generation-0001"), "workspace-0001", 2_000)
                .unwrap(),
            "workspace-0001"
        );
        assert_eq!(
            manager
                .workspace_for_task("1", &context("generation-0001"), "workspace-other", 2_000)
                .unwrap_err()
                .code,
            "capability_workspace_not_authorized"
        );
    }

    #[test]
    fn descriptor_input_is_bounded_and_rejects_duplicates_or_paths() {
        let manager = CapabilitySnapshotManager::default();
        let mut source = fixture_source();
        source.tools = vec![
            CapabilityDescriptor {
                id: "tool-one".into(),
                display_name: "One".into(),
            },
            CapabilityDescriptor {
                id: "tool-one".into(),
                display_name: "Duplicate".into(),
            },
        ];
        assert_eq!(
            manager
                .build(context("generation-0001"), source, 1_000)
                .unwrap_err()
                .code,
            "capability_descriptor_duplicate"
        );

        let mut invalid = fixture_source();
        invalid.tools = vec![CapabilityDescriptor {
            id: r"D:\tool".into(),
            display_name: "Path".into(),
        }];
        assert_eq!(
            manager
                .build(context("generation-0001"), invalid, 1_000)
                .unwrap_err()
                .code,
            "capability_id_invalid"
        );
    }

    /// E2E-04：wire 投影必须是冻结形状，且通过运行时校验器（角色 B + 租约 +
    /// 协商能力），否则 A 端会在解密后再被拒。
    #[test]
    fn wire_capability_snapshot_matches_the_frozen_protocol_shape() {
        let manager = CapabilitySnapshotManager::default();
        let lease = context("generation-0001");
        let snapshot = manager
            .build(lease.clone(), fixture_source(), 1_000)
            .unwrap();
        let mut payload = manager.wire_capability_snapshot(&snapshot, "request-snapshot-1");
        // The business adapter binds both session identities before encryption.
        payload["session_id"] = serde_json::json!("remote-ref-1");
        payload["controlled_session_id"] = serde_json::json!("b-session-1");
        let capabilities = vec![
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "task_v1".to_owned(),
        ];
        let encoded = payload.to_string();
        super::super::protocol::validate_controller_runtime_payload_json(
            &encoded,
            super::super::protocol::SenderRole::ControlledB,
            Some(&super::super::protocol::RuntimeValidationContext {
                lease: Some(lease),
                negotiated_capabilities: capabilities,
            }),
        )
        .expect("wire capability snapshot must satisfy the frozen runtime contract");
        assert_eq!(payload["type"], "capability_snapshot");
        assert_eq!(payload["request_id"], "request-snapshot-1");
        assert_eq!(payload["session_id"], "remote-ref-1");
        assert_eq!(payload["controlled_session_id"], "b-session-1");
        assert_eq!(payload["capability_revision"], "1");
        // 序列化文本不得包含宿主路径或凭证关键字。
        let text = payload.to_string();
        assert!(!text.contains("D:\\"));
        assert!(!text.contains("api_key"));
        assert_eq!(payload["skills"][0]["scopes"], serde_json::json!(["code"]));
    }
}
