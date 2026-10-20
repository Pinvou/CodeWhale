//! Only definite pre-admission failures may retire A's pending request.
//! Timeouts, worker/persistence failures and ledger conflicts can be ambiguous.
pub(crate) const CAPACITY: &str = "task_rejection_capacity";

pub(crate) const CODES: &[&str] = &[
    CAPACITY,
    "task_permission_mode_required",
    "task_permission_mode_changed",
    "task_permission_mode_unsupported",
    "task_session_busy",
    "task_turn_timeout_config_invalid",
    "task_session_not_found",
    "task_session_generation_stale",
    "task_session_generation_conflict",
    "task_session_map_conflict",
    "task_session_lane_conflict",
    "task_session_lane_unknown",
    "task_session_lane_mismatch",
    "task_session_binding_mismatch",
    "task_lease_mismatch",
    "task_session_provisioner_unavailable",
    "task_session_backend_bind_failed",
    "task_session_create_failed",
    "task_session_lookup_failed",
    "task_request_invalid",
    "task_binding_stale",
    "task_not_found",
    "task_input_digest_invalid",
    "task_input_invalid",
    "task_skill_invalid",
    "task_mode_unsupported",
    "capability_snapshot_missing",
    "capability_revision_exhausted",
    "capability_admission_capacity",
    "capability_expiry_overflow",
    "capability_workspace_capacity",
    "capability_workspace_duplicate",
    "capability_descriptor_capacity",
    "capability_descriptor_duplicate",
    "capability_id_invalid",
    "capability_display_invalid",
    "temporary_model_capability_required",
    "task_model_source_unsupported",
    "model_package_missing",
    "model_not_exportable",
    "task_workspace_required",
    "task_workspace_not_authorized",
    "task_workspace_unexpected",
    "task_mode_invalid",
    "task_model_source_invalid",
    "task_workspace_capacity",
    "capability_snapshot_stale",
    "capability_snapshot_expired",
    "capability_workspace_not_authorized",
    "desktop_remote_not_connected",
    "lease_unavailable",
    "model_binding_invalid",
    "model_binding_stale",
    "model_credential_unavailable",
    "model_credential_invalid",
    "model_credential_kind_unsupported",
    "model_display_name_invalid",
    "model_endpoint_insecure",
    "model_endpoint_invalid",
    "model_endpoint_local",
    "model_endpoint_mode_unsupported",
    "model_endpoint_private",
    "model_endpoint_unresolved",
    "model_image_override_unsupported",
    "model_package_capacity",
    "model_package_field_invalid",
    "model_package_id_invalid",
    "model_package_invalid",
    "model_package_missing_field",
    "model_package_schema_unsupported",
    "model_package_too_large",
    "model_package_unknown_field",
    "model_preset_unsupported",
    "model_provider_kind_unsupported",
    "model_reasoning_effort_unsupported",
    "model_route_invalid",
    "model_runtime_unavailable",
    "model_secret_invalid",
    "model_secret_too_large",
    "model_vendor_unsupported",
    "model_wire_api_mismatch",
    "model_wire_api_unsupported",
    "model_wire_name_invalid",
    "controlled_session_projection_failed",
    "task_payload_invalid_json",
    "task_payload_invalid_message",
    "task_payload_invalid_type",
    "task_payload_invalid_field",
    "task_payload_unknown_field",
    "task_payload_invalid_enum",
    "task_payload_length_mismatch",
    "task_payload_missing_field",
    "task_payload_unknown_message_type",
    "task_payload_message_too_large",
    "task_payload_unsupported_version",
    "task_payload_unsupported_capability",
    "task_payload_stale_context",
    "task_payload_role_mismatch",
    "task_payload_replay",
];

pub(crate) fn is_definite(code: &str) -> bool {
    CODES.contains(&code)
}

pub(super) const RESPONSE_CONTEXT: &[&str] = &[
    "connection_id",
    "controller_lease_id",
    "lease_epoch",
    "relay_epoch",
    "request_id",
];

pub(super) fn fingerprint(payload: &serde_json::Value) -> [u8; 32] {
    use sha2::{Digest, Sha256};
    let mut stable = payload.clone();
    if let Some(object) = stable.as_object_mut() {
        for key in RESPONSE_CONTEXT {
            object.remove(*key);
        }
    }
    // Generation, session, both task IDs and all input semantics remain bound;
    // a resumed transport may only replace its response routing context.
    Sha256::digest(stable.to_string().as_bytes()).into()
}

/// Remember every attempted task for this generation, including accepted and
/// ambiguous outcomes. A pruned ledger record is not proof of non-execution.
/// Never evict: a duplicate must not become a first attempt after a policy change.
#[derive(Default)]
pub(super) struct Cache(
    parking_lot::Mutex<std::collections::HashMap<String, ([u8; 32], Option<serde_json::Value>)>>,
);

pub(super) enum Lookup {
    First,
    Retry,
    Refused(serde_json::Value),
    Capacity,
}

impl Cache {
    pub(super) fn begin(&self, task_id: &str, fingerprint: [u8; 32]) -> Result<Lookup, String> {
        let mut entries = self.0.lock();
        match entries.get(task_id) {
            Some((previous, Some(response))) if previous == &fingerprint => {
                Ok(Lookup::Refused(response.clone()))
            }
            Some((previous, None)) if previous == &fingerprint => Ok(Lookup::Retry),
            Some(_) => Err("task_request_conflict".into()),
            None if entries.len() >= 4096 => Ok(Lookup::Capacity),
            None => {
                entries.insert(task_id.to_owned(), (fingerprint, None));
                Ok(Lookup::First)
            }
        }
    }

    pub(super) fn refuse(&self, task_id: &str, response: serde_json::Value) {
        if let Some((_, outcome)) = self.0.lock().get_mut(task_id) {
            *outcome = Some(response);
        }
    }
}

pub(super) fn response(
    payload: &serde_json::Value,
    code: &str,
) -> Result<serde_json::Value, String> {
    let mut response = payload.clone();
    let object = response
        .as_object_mut()
        .ok_or_else(|| "task_request_invalid".to_owned())?;
    object.retain(|key, _| {
        [
            "v",
            "connection_id",
            "controller_lease_id",
            "lease_epoch",
            "relay_epoch",
            "remote_control_generation",
            "request_id",
            "task_id",
            "client_request_id",
            "session_id",
        ]
        .contains(&key.as_str())
    });
    object.insert("type".into(), serde_json::json!("task_status"));
    object.insert("task_status".into(), serde_json::json!("failed"));
    object.insert("rejection_code".into(), serde_json::json!(code));
    Ok(response)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn attempt_evidence_is_bounded_and_never_evicted_or_changed() {
        let cache = Cache::default();
        for index in 0..4096 {
            assert!(matches!(
                cache.begin(&format!("task-{index}"), [1; 32]).unwrap(),
                Lookup::First
            ));
        }
        assert!(matches!(
            cache.begin("overflow", [1; 32]).unwrap(),
            Lookup::Capacity
        ));
        assert!(matches!(
            cache.begin("task-1", [1; 32]).unwrap(),
            Lookup::Retry
        ));
        assert!(cache.begin("task-1", [2; 32]).is_err());
        cache.refuse(
            "task-0",
            serde_json::json!({"rejection_code":"task_permission_mode_required"}),
        );
        assert!(matches!(
            cache.begin("task-0", [1; 32]).unwrap(),
            Lookup::Refused(_)
        ));
        for code in [
            "task_turn_timeout",
            "controlled_submission_worker_failed",
            "task_ledger_write_failed",
            "task_request_conflict",
            "task_status_binding_invalid",
        ] {
            assert!(!is_definite(code));
        }
    }
}
