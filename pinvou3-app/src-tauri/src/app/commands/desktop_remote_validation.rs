//! Pure validation shared by remote host adapters. No AppHandle, state lookup,
//! lock acquisition or side effects live here; each caller retains its own
//! current-owner/peer/resume fence and locked recheck around the commit.

use crate::features::remote_control::controller::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_payload_json,
};
use serde_json::Value;
use std::collections::HashSet;

pub(super) fn capability_allows_skills(
    capability: &crate::features::remote_control::controller::controlling_session::ControllerCapabilityProjection,
    mode: &str,
    skill_ids: &[String],
) -> bool {
    let mut seen = HashSet::new();
    skill_ids.len() <= 32
        && skill_ids.iter().all(|skill_id| {
            seen.insert(skill_id.as_str())
                && capability.skills.iter().any(|skill| {
                    skill.skill_id == *skill_id && skill.scopes.iter().any(|scope| scope == mode)
                })
        })
}

pub(super) fn validated_task_payload(
    payload: &Value,
    lease: &LeaseContext,
    negotiated_capabilities: &[String],
) -> Result<Value, String> {
    let encoded = serde_json::to_string(payload).map_err(|_| "task_request_invalid".to_owned())?;
    let context = RuntimeValidationContext {
        lease: Some(lease.clone()),
        negotiated_capabilities: negotiated_capabilities.to_vec(),
    };
    let validated =
        validate_controller_runtime_payload_json(&encoded, SenderRole::ControllerA, Some(&context))
            .map_err(|error| format!("task_payload_{}", error.code))?;
    if validated.get("type").and_then(Value::as_str) != Some("task_submit") {
        return Err("task_request_invalid".to_owned());
    }
    Ok(validated)
}

pub(super) fn lease_matches_remote_binding(
    lease: &LeaseContext,
    live: &crate::features::sessions::remote_proxy::binding::LiveRemoteContext,
) -> bool {
    lease.connection_id == live.connection_id
        && lease.controller_lease_id == live.controller_lease_id
        && lease.lease_epoch == live.lease_epoch
        && lease.relay_epoch == live.relay_epoch
        && lease.remote_control_generation == live.generation
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;

    #[test]
    fn every_lease_field_is_required_even_with_the_same_connection() {
        let live = LiveRemoteContext {
            peer_id: "peer".into(),
            resume_boundary_id: "resume".into(),
            connection_id: "connection".into(),
            controller_lease_id: "lease".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay".into(),
            generation: "generation".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        };
        let lease = LeaseContext {
            connection_id: live.connection_id.clone(),
            controller_lease_id: live.controller_lease_id.clone(),
            lease_epoch: live.lease_epoch.clone(),
            relay_epoch: live.relay_epoch.clone(),
            remote_control_generation: live.generation.clone(),
        };
        assert!(lease_matches_remote_binding(&lease, &live));
        let change: [fn(&mut LeaseContext); 5] = [
            |v| v.connection_id.push('x'),
            |v| v.controller_lease_id.push('x'),
            |v| v.lease_epoch.push('x'),
            |v| v.relay_epoch.push('x'),
            |v| v.remote_control_generation.push('x'),
        ];
        for mutate in change {
            let mut other = lease.clone();
            mutate(&mut other);
            assert!(!lease_matches_remote_binding(&other, &live));
        }
    }
}
