//! Minimal, revisioned B capability snapshot.
//!
//! A snapshot is a display/admission hint, never an authorization bypass. The
//! B task owner must revalidate the exact workspace and tool policy again when
//! admitting work. No source path, Skill body, environment variable, credential,
//! or package content crosses this boundary.

use std::collections::HashSet;

use parking_lot::Mutex;
use serde::Serialize;

use super::controlled::ControlledWorkspaceInfo;
use super::protocol::LeaseContext;

const SNAPSHOT_TTL_MS: u64 = 30_000;
const MAX_DESCRIPTORS: usize = 256;
const MAX_ID_CHARS: usize = 128;
const MAX_DISPLAY_CHARS: usize = 160;

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
        let mut skills = source.plain_skills.clone();
        if code_mode_allowed {
            skills.extend(source.code_skills.iter().cloned());
        }
        skills.sort_by(|left, right| {
            left.id
                .cmp(&right.id)
                .then(left.display_name.cmp(&right.display_name))
        });
        skills.dedup_by(|left, right| left.id == right.id);

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
                .map(|skill| RemoteCapabilitySkill {
                    skill_id: skill.id,
                    display_name: skill.display_name,
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
        let snapshot = self
            .current
            .lock()
            .clone()
            .ok_or_else(|| CapabilityError::new("capability_snapshot_missing"))?;
        if snapshot.capability_revision != revision
            || snapshot.connection_id != context.connection_id
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
        assert_eq!(snapshot.workspaces.len(), 1);
        assert_eq!(
            manager
                .validate("1", &context("generation-0001"), 2_000)
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
}
