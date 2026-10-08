//! Minimal, revisioned B capability snapshot.
//!
//! A snapshot is a display/admission hint, never an authorization bypass. The
//! B task owner must revalidate the exact workspace and tool policy again when
//! admitting work. No source path, Skill body, environment variable, credential,
//! or package content crosses this boundary.

use std::collections::{BTreeMap, HashSet};

use parking_lot::Mutex;
use serde::Serialize;
use sha2::{Digest, Sha256};

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
    state: Mutex<CapabilityState>,
}

#[derive(Default)]
struct CapabilityState {
    revision: u64,
    current: Option<PublishedSnapshot>,
}

struct PublishedSnapshot {
    snapshot: CapabilitySnapshot,
    // B-only directory identities, never serialized or supplied by A.
    skills: BTreeMap<String, SkillBinding>,
}

struct SkillBinding {
    name: String,
    display_name: String,
    modes: HashSet<&'static str>,
}

/// Stable identity over exact UTF-8 directory names, not the editable title.
/// Scope and live enablement are checked separately on B. No path or Skill body
/// is encoded in this ID, and Unicode names are deliberately not normalized.
pub(crate) fn opaque_skill_id(name: &str) -> String {
    let mut hash = Sha256::new();
    hash.update(b"pinvou.remote-control.skill-id.v1\0");
    hash.update((name.len() as u64).to_be_bytes());
    hash.update(name.as_bytes());
    let mut id = String::from("skill-");
    const HEX: &[u8; 16] = b"0123456789abcdef";
    for byte in hash.finalize() {
        id.push(HEX[(byte >> 4) as usize] as char);
        id.push(HEX[(byte & 15) as usize] as char);
    }
    id
}

impl CapabilitySnapshotManager {
    pub(crate) fn build(
        &self,
        context: LeaseContext,
        source: CapabilitySource,
        now_b: u64,
    ) -> Result<CapabilitySnapshot> {
        validate_descriptors("tool", &source.tools)?;
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
        let mut bindings = BTreeMap::new();
        project_skills(&mut bindings, &source.plain_skills, "chat")?;
        if code_mode_allowed {
            project_skills(&mut bindings, &source.code_skills, "code")?;
        } else {
            // Even hidden code-source input must satisfy the local boundary.
            project_skills(&mut BTreeMap::new(), &source.code_skills, "code")?;
        }
        if bindings.len() > MAX_DESCRIPTORS {
            return Err(CapabilityError::new("capability_descriptor_capacity"));
        }
        let expires_at_ms = now_b
            .checked_add(SNAPSHOT_TTL_MS)
            .ok_or_else(|| CapabilityError::new("capability_expiry_overflow"))?;
        // Revision, wire projection and private bindings are published together.
        // A separate map or revision lock could pair an old snapshot with a new
        // mapping during concurrent refresh.
        let mut state = self.state.lock();
        let revision = state
            .revision
            .checked_add(1)
            .ok_or_else(|| CapabilityError::new("capability_revision_exhausted"))?;
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
            skills: bindings
                .iter()
                .map(|(id, skill)| RemoteCapabilitySkill {
                    skill_id: id.clone(),
                    display_name: skill.display_name.clone(),
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
        state.revision = revision;
        state.current = Some(PublishedSnapshot {
            snapshot: snapshot.clone(),
            skills: bindings,
        });
        Ok(snapshot)
    }

    pub(crate) fn validate(
        &self,
        revision: &str,
        context: &LeaseContext,
        now_b: u64,
    ) -> Result<CapabilitySnapshot> {
        let state = self.state.lock();
        Ok(validated_snapshot(&state, revision, context, now_b)?
            .snapshot
            .clone())
    }

    /// Resolve A's opaque selection to B's original names under the exact
    /// snapshot/lease/mode. The command/runtime must recheck live enablement;
    /// this mapping is a bounded admission hint, never a permission grant.
    pub(crate) fn skills_for_task(
        &self,
        revision: &str,
        context: &LeaseContext,
        mode: &str,
        ids: &[String],
        now_b: u64,
    ) -> Result<Vec<String>> {
        let state = self.state.lock();
        let published = validated_snapshot(&state, revision, context, now_b)?;
        if ids.len() > 32 || !published.snapshot.modes.iter().any(|value| value == mode) {
            return Err(CapabilityError::new("task_skill_invalid"));
        }
        let mut seen = HashSet::new();
        ids.iter()
            .map(|id| {
                if !seen.insert(id.as_str()) {
                    return Err(CapabilityError::new("task_skill_invalid"));
                }
                published
                    .skills
                    .get(id)
                    .filter(|binding| binding.modes.contains(mode))
                    .map(|binding| binding.name.clone())
                    .ok_or_else(|| CapabilityError::new("task_skill_invalid"))
            })
            .collect()
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

fn validated_snapshot<'a>(
    state: &'a CapabilityState,
    revision: &str,
    context: &LeaseContext,
    now_b: u64,
) -> Result<&'a PublishedSnapshot> {
    let published = state
        .current
        .as_ref()
        .ok_or_else(|| CapabilityError::new("capability_snapshot_missing"))?;
    let snapshot = &published.snapshot;
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
    Ok(published)
}

fn project_skills(
    bindings: &mut BTreeMap<String, SkillBinding>,
    values: &[CapabilityDescriptor],
    mode: &'static str,
) -> Result<()> {
    if values.len() > MAX_DESCRIPTORS {
        return Err(CapabilityError::new("capability_descriptor_capacity"));
    }
    let mut names = HashSet::new();
    for value in values {
        // The source is a local directory name, NOT a wire identifier.
        if value.id.is_empty()
            || value.id.len() > 1024
            || matches!(value.id.as_str(), "." | "..")
            || value
                .id
                .chars()
                .any(|ch| ch.is_control() || matches!(ch, '/' | '\\'))
        {
            return Err(CapabilityError::new("capability_skill_name_invalid"));
        }
        validate_display(&value.display_name)?;
        if !names.insert(value.id.as_str()) {
            return Err(CapabilityError::new("capability_descriptor_duplicate"));
        }
        let id = opaque_skill_id(&value.id);
        let binding = bindings.entry(id).or_insert_with(|| SkillBinding {
            name: value.id.clone(),
            display_name: value.display_name.clone(),
            modes: HashSet::new(),
        });
        if binding.name != value.id || binding.display_name != value.display_name {
            return Err(CapabilityError::new("capability_skill_identity_conflict"));
        }
        binding.modes.insert(mode);
    }
    Ok(())
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
    fn unicode_skill_directory_does_not_disable_the_entire_snapshot() {
        let manager = CapabilitySnapshotManager::default();
        let mut source = fixture_source();
        source.plain_skills = vec![CapabilityDescriptor {
            id: "我的技能".into(),
            display_name: "中文技能".into(),
        }];
        let snapshot = manager
            .build(context("generation-0001"), source.clone(), 1_000)
            .unwrap();
        let skill = snapshot
            .skills
            .iter()
            .find(|skill| skill.display_name == "中文技能")
            .unwrap();
        assert!(skill.skill_id.is_ascii());
        assert_ne!(skill.skill_id, "我的技能");
        assert!(snapshot.modes.iter().any(|mode| mode == "chat"));
        let refreshed = manager
            .build(context("generation-0001"), source, 2_000)
            .unwrap();
        assert_eq!(
            skill.skill_id,
            refreshed
                .skills
                .iter()
                .find(|skill| skill.display_name == "中文技能")
                .unwrap()
                .skill_id
        );
        assert!(
            !serde_json::to_string(&snapshot)
                .unwrap()
                .contains("我的技能")
        );
    }

    #[test]
    fn skill_mapping_is_exact_revision_lease_and_mode_bound() {
        let manager = CapabilitySnapshotManager::default();
        let lease = context("generation-0001");
        let mut source = fixture_source();
        source.plain_skills[0].id = "我的技能".into();
        manager.build(lease.clone(), source.clone(), 1_000).unwrap();
        let id = opaque_skill_id("我的技能");
        assert_eq!(
            manager
                .skills_for_task("1", &lease, "chat", std::slice::from_ref(&id), 2_000)
                .unwrap(),
            vec!["我的技能"]
        );
        for (mode, ids) in [
            ("code", vec![id.clone()]),
            ("chat", vec![opaque_skill_id("不存在")]),
            ("chat", vec!["我的技能".into()]),
            ("chat", vec![id.clone(), id.clone()]),
        ] {
            assert_eq!(
                manager
                    .skills_for_task("1", &lease, mode, &ids, 2_000)
                    .unwrap_err()
                    .code,
                "task_skill_invalid"
            );
        }
        assert_eq!(
            manager
                .skills_for_task(
                    "1",
                    &context("new-generation"),
                    "chat",
                    std::slice::from_ref(&id),
                    2_000
                )
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        assert_eq!(
            manager
                .skills_for_task("1", &lease, "chat", std::slice::from_ref(&id), 31_000)
                .unwrap_err()
                .code,
            "capability_snapshot_expired"
        );
        source.plain_skills[0].display_name = "新标题".into();
        manager.build(lease.clone(), source, 3_000).unwrap();
        assert_eq!(
            manager
                .skills_for_task("1", &lease, "chat", std::slice::from_ref(&id), 3_001)
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        assert_eq!(
            manager
                .skills_for_task("2", &lease, "chat", std::slice::from_ref(&id), 3_001)
                .unwrap(),
            vec!["我的技能"]
        );
        assert_ne!(opaque_skill_id("é"), opaque_skill_id("e\u{301}"));
    }

    #[test]
    fn skill_projection_bounds_and_validates_local_names_without_relaxing_wire_ids() {
        let manager = CapabilitySnapshotManager::default();
        for name in ["", ".", "..", "../技能", "folder\\技能", "技能\n"] {
            let mut source = fixture_source();
            source.plain_skills[0].id = name.into();
            assert_eq!(
                manager
                    .build(context("generation-0001"), source, 1_000)
                    .unwrap_err()
                    .code,
                "capability_skill_name_invalid"
            );
        }
        let mut source = fixture_source();
        source.plain_skills.push(source.plain_skills[0].clone());
        assert_eq!(
            manager
                .build(context("generation-0001"), source, 1_000)
                .unwrap_err()
                .code,
            "capability_descriptor_duplicate"
        );
        let mut source = fixture_source();
        source.plain_skills = (0..MAX_DESCRIPTORS)
            .map(|index| CapabilityDescriptor {
                id: format!("技能-{index}"),
                display_name: format!("技能 {index}"),
            })
            .collect();
        assert_eq!(
            manager
                .build(context("generation-0001"), source, 1_000)
                .unwrap_err()
                .code,
            "capability_descriptor_capacity"
        );
    }

    #[test]
    fn concurrent_refresh_cannot_publish_older_revision_or_mismatched_mapping() {
        let manager = std::sync::Arc::new(CapabilitySnapshotManager::default());
        let revisions: Vec<_> = (0..16)
            .map(|index| {
                let manager = manager.clone();
                std::thread::spawn(move || {
                    let mut source = fixture_source();
                    source.plain_skills[0].id = format!("技能-{index}");
                    let snapshot = manager
                        .build(context("generation-0001"), source, 1_000)
                        .unwrap();
                    (snapshot.capability_revision.parse::<u64>().unwrap(), index)
                })
            })
            .collect();
        let (revision, index) = revisions
            .into_iter()
            .map(|thread| thread.join().unwrap())
            .max()
            .unwrap();
        let name = format!("技能-{index}");
        assert_eq!(revision, 16);
        assert_eq!(
            manager
                .skills_for_task(
                    &revision.to_string(),
                    &context("generation-0001"),
                    "chat",
                    &[opaque_skill_id(&name)],
                    2_000
                )
                .unwrap(),
            vec![name]
        );
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
