//! Windows remote capability snapshot commands.
//!
//! The command reads B's installed/enabled marketplace surfaces and ACP
//! readiness, then passes only opaque IDs and display names through the DEV-12
//! snapshot manager. DEV-20 owns final handler registration.

use std::collections::{HashMap, HashSet};
use std::sync::OnceLock;
use std::time::Instant;

use tauri::{AppHandle, Manager as _};

use crate::features::assistant::skill_materialization::enabled_skills_for;
use crate::features::codex_acp::AcpPool;
use crate::features::marketplace::skill_marketplace::SkillMarketplaceManager;
use crate::features::marketplace::{ConnectorScope, MarketplaceManager, unavailable_bundles_for};
use crate::features::remote_control::controller::capabilities::{
    CapabilityDescriptor, CapabilityError, CapabilitySnapshot, CapabilitySnapshotManager,
    CapabilitySource,
};
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

fn capability_error(error: CapabilityError) -> String {
    error.code.to_owned()
}

fn monotonic_millis() -> u64 {
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

fn descriptor(id: impl Into<String>, display_name: impl Into<String>) -> CapabilityDescriptor {
    CapabilityDescriptor {
        id: id.into(),
        display_name: display_name.into().trim().to_owned(),
    }
}

fn marketplace_tool_projection() -> Vec<CapabilityDescriptor> {
    let unavailable_plain: HashSet<String> = unavailable_bundles_for(ConnectorScope::Plain)
        .into_iter()
        .collect();
    let unavailable_code: HashSet<String> = unavailable_bundles_for(ConnectorScope::Code)
        .into_iter()
        .collect();
    MarketplaceManager::new()
        .list_tools()
        .into_iter()
        .filter(|tool| tool.installed)
        .filter(|tool| {
            !unavailable_plain.contains(&tool.id) && !unavailable_code.contains(&tool.id)
        })
        .map(|tool| descriptor(tool.id, tool.name))
        .collect()
}

fn enabled_skill_projection(
    scope: ConnectorScope,
    display_names: &HashMap<String, String>,
) -> Vec<CapabilityDescriptor> {
    enabled_skills_for(scope, None)
        .into_iter()
        .map(|(name, _path)| {
            let display = display_names
                .get(&name)
                .cloned()
                .unwrap_or_else(|| name.clone());
            descriptor(name, display)
        })
        .collect()
}

#[tauri::command]
pub async fn desktop_remote_capabilities_snapshot(
    app: AppHandle,
) -> Result<CapabilitySnapshot, String> {
    build_capability_snapshot(&app).await
}

/// 构造 B 端能力快照：命令入口（本地 UI）与 wire 派发入口（E2E-04 的
/// `capability_snapshot_request`）共用同一份实现。
pub(crate) async fn build_capability_snapshot(app: &AppHandle) -> Result<CapabilitySnapshot, String> {
    let controlled = app.state::<ControlledDeviceManager>();
    if !controlled.is_supported() {
        return Err(UNSUPPORTED.to_owned());
    }
    let context = controlled
        .live_lease()
        .map_err(|error| error.code.to_owned())?;
    let acp_pool = app.state::<AcpPool>();
    let code_agent_ready = acp_pool
        .status_for_agent("codex")
        .await
        .map(|status| status.installed && status.bridge_ready)
        .unwrap_or(false);
    let skill_display_names: HashMap<String, String> = SkillMarketplaceManager::new()
        .list_skills()
        .into_iter()
        .filter(|skill| skill.installed)
        .map(|skill| (skill.id, skill.title))
        .collect();
    let source = CapabilitySource {
        code_agent_ready,
        tools: marketplace_tool_projection(),
        plain_skills: enabled_skill_projection(ConnectorScope::Plain, &skill_display_names),
        code_skills: enabled_skill_projection(ConnectorScope::Code, &skill_display_names),
        workspaces: controlled.workspaces(),
    };
    app.state::<CapabilitySnapshotManager>()
        .build(context, source, monotonic_millis())
        .map_err(capability_error)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn descriptor_projection_trims_display_text_and_keeps_ids_opaque() {
        let value = descriptor("tool.one", "  Browser  ");
        assert_eq!(value.id, "tool.one");
        assert_eq!(value.display_name, "Browser");
        assert!(!format!("{value:?}").contains(r"D:\\"));
    }
}
