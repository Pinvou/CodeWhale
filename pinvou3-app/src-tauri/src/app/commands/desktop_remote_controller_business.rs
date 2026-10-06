//! B 端业务端口实现（E2E-04）。
//!
//! 握手完成后的 worker 只负责解密/映射/回传，真正的业务动作（能力快照、
//! 任务接纳、任务取消）在这里落到既有的命令层实现上：三条路径都复用
//! `desktop_remote_task` / `desktop_remote_capabilities` 的同一套逻辑，避免
//! 「命令入口」与「wire 入口」两份实现漂移。

use crate::features::codex_acp::{AcpPool, RemotePermissionOwner};
use serde_json::{Value, json};

use crate::features::remote_control::controller::capabilities::CapabilitySnapshotManager;
use crate::features::remote_control::controller::controlled::{
    ControlledCleanupScope, ControlledCleanupSink, ControlledDeviceManager,
};
use crate::features::remote_control::controller::permission::{
    DecisionAuthority, DecisionOutcome, DispatchCommit, DispatchState, PermissionAuthority,
    PermissionDecision,
};
use crate::features::remote_control::controller::ports::{
    ControlledExecutionPort, RemotePortError, RemotePortFuture,
};
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::protocol::{
    MAX_TASK_INPUT_TEXT_BYTES, RuntimeValidationContext, SenderRole,
    validate_controller_runtime_payload_json,
};
use crate::features::remote_control::controller::relay_runtime::{
    ControlledBusinessPort, controlled_monotonic_millis, log_controlled_port_error,
};
use crate::features::remote_control::controller::task::{
    RemoteTaskLedger, RemoteTaskState, TaskLeaseBinding,
};
use crate::features::remote_control::controller::task::{
    RemoteTaskRequest, TASK_MODE_CHAT, TASK_MODE_CODE, TASK_MODEL_SOURCE_CONTROLLED,
};
use crate::features::remote_control::controller::transfer::core::{
    TransferContext, TransferDirection, TransferMetadata,
};
use crate::features::remote_control::controller::transfer::upload::{
    UploadProgress, decode_upload_chunk,
};
use crate::features::sessions::application::ports::{UseCaseError, UseCaseFuture};
use crate::features::sessions::application::submit_controlled::{
    ControlledInput, ControlledSubmission, ControlledSubmissionPort, ControlledSubmissionResult,
    LocalControlledInput, PreparedControlledTask, RemoteControlledInput, SubmitControlledUseCase,
};
use crate::features::sessions::remote_proxy::outbox::remote_task_input_digest;
use crate::features::sessions::remote_proxy::schema::valid_remote_identifier;
use base64::Engine as _;
use std::collections::HashSet;
use std::sync::Arc;
use tauri::{AppHandle, Manager as _};

/// A same-value allow retry observes the original dispatch, never dispatches twice.
fn commit_effective_permission_allow(
    authority: &PermissionAuthority,
    request_id: &str,
    outcome: &DecisionOutcome,
) -> Result<(), String> {
    match outcome {
        DecisionOutcome::Decided {
            decision: PermissionDecision::Allow,
            dispatch_state,
        }
        | DecisionOutcome::Replayed {
            decision: PermissionDecision::Allow,
            dispatch_state,
        } => {
            if *dispatch_state == DispatchState::Dispatched {
                return Ok(());
            }
            match authority
                .commit_dispatch(request_id, authority.now_ms())
                .map_err(|error| error.code.to_owned())?
            {
                DispatchCommit::Committed
                | DispatchCommit::Rejected {
                    code: "permission_already_dispatched",
                } => Ok(()),
                DispatchCommit::Rejected { code } => Err(code.to_owned()),
            }
        }
        _ => Ok(()),
    }
}

/// E2E-06：B 端清理端口。`ControlledDeviceManager` 在 stop / refresh / 远端断开 /
/// 恢复失败等任何终态都会触发它；这里用同一代的任务登记表中断本代远端任务，
/// 任务生命周期钩子会顺带回收临时模型凭证、待决权限与传输暂存
/// （`controller/task.rs` 的 `RemoteTaskService::cleanup`）。
pub(crate) struct DesktopControlledCleanup {
    app: AppHandle,
}

impl DesktopControlledCleanup {
    pub(crate) fn new(app: AppHandle) -> Self {
        Self { app }
    }
}

impl ControlledCleanupSink for DesktopControlledCleanup {
    fn cleanup(&self, generation: &str, scope: ControlledCleanupScope) {
        // Registry entries belong to one authenticated generation. An old
        // cleanup cannot clear a newly paired generation's authorizations.
        if scope.workspace_grants || scope.remote_tasks {
            if let Some(runtime) = self
                .app
                .try_state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>()
            {
                runtime.authorizations().clear_generation(generation);
            }
        }
        if scope.session_keys {
            if let Some(relay) = self
                .app
                .try_state::<crate::features::remote_control::RemoteControlManager>()
            {
                relay.stop_controller_relay(generation);
            }
        }
        // 审核修复：只有涉及远端任务/待决权限/临时模型的清理才需要中断任务；
        // 纯凭证（session_keys/workspace_grants）清理不得顺带打断任务。
        if !(scope.remote_tasks || scope.pending_permissions || scope.temporary_models) {
            return;
        }
        let Some(runtime) = self
            .app
            .try_state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>()
        else {
            // 任务运行时未装配（例如单元测试环境）：本代没有任何远端任务需要清理。
            return;
        };
        let now_b = controlled_monotonic_millis();
        let _ = runtime.service().interrupt_generation_and_clear_sessions(
            generation,
            "remote_control_stopped",
            now_b,
        );
    }
}

/// 绑定 app handle 的 B 端业务端口（每个连接一个实例）。
pub(crate) struct DesktopControlledBusiness {
    pub(crate) app: AppHandle,
}

/// Composition-root adapter: no ledger, mapping or executor state lives here.
/// Both sources enter the same host-independent use case, then the existing
/// DEV-13 service admits and dispatches exactly once.
pub(crate) struct DesktopControlledExecutionAdapter {
    app: AppHandle,
}

impl DesktopControlledExecutionAdapter {
    pub(crate) fn new(app: AppHandle) -> Self {
        Self { app }
    }

    /// B ChatView's backend entry; the UI cannot supply task/request IDs or
    /// an input digest. The existing registered command can delegate here
    /// once its DEV-29 owner has finished editing that shared file.
    pub(crate) async fn submit_local(
        &self,
        request: LocalControlledInput,
    ) -> Result<ControlledSubmissionResult, RemotePortError> {
        let use_case = ControlledSubmission::new(DesktopControlledSubmitBackend {
            app: self.app.clone(),
        });
        use_case
            .submit(ControlledInput::Local(request))
            .await
            .map_err(|error| RemotePortError::new(error.code(), error.is_retryable()))
    }
}

fn submit_error(code: &str) -> UseCaseError {
    match code {
        "task_session_busy" => UseCaseError::new("task_session_busy", true),
        "task_session_not_found" => UseCaseError::new("task_session_not_found", false),
        "task_session_generation_stale" => UseCaseError::new("task_session_generation_stale", true),
        "task_session_generation_conflict" => {
            UseCaseError::new("task_session_generation_conflict", false)
        }
        "task_session_map_conflict" => UseCaseError::new("task_session_map_conflict", false),
        "task_session_lane_conflict" => UseCaseError::new("task_session_lane_conflict", false),
        "task_session_lane_unknown" => UseCaseError::new("task_session_lane_unknown", false),
        "task_session_lane_mismatch" => UseCaseError::new("task_session_lane_mismatch", false),
        "task_session_binding_mismatch" => {
            UseCaseError::new("task_session_binding_mismatch", false)
        }
        "task_lease_mismatch" => UseCaseError::new("task_lease_mismatch", true),
        "task_session_provisioner_unavailable" => {
            UseCaseError::new("task_session_provisioner_unavailable", true)
        }
        "task_session_backend_bind_failed" => {
            UseCaseError::new("task_session_backend_bind_failed", true)
        }
        "task_session_create_failed" => UseCaseError::new("task_session_create_failed", true),
        "task_session_lookup_failed" => UseCaseError::new("task_session_lookup_failed", true),
        "task_request_conflict" => UseCaseError::new("task_request_conflict", false),
        "task_request_invalid" => UseCaseError::new("task_request_invalid", false),
        "task_binding_stale" => UseCaseError::new("task_binding_stale", true),
        "task_ledger_capacity" => UseCaseError::new("task_ledger_capacity", true),
        "task_ledger_persist_failed" => UseCaseError::new("task_ledger_persist_failed", true),
        "task_ledger_unreadable" => UseCaseError::new("task_ledger_unreadable", false),
        "task_ledger_corrupt" => UseCaseError::new("task_ledger_corrupt", false),
        "task_ledger_version_unsupported" => {
            UseCaseError::new("task_ledger_version_unsupported", false)
        }
        "task_ledger_serialize_failed" => UseCaseError::new("task_ledger_serialize_failed", false),
        "task_ledger_write_failed" => UseCaseError::new("task_ledger_write_failed", true),
        "task_not_found" => UseCaseError::new("task_not_found", false),
        "task_state_conflict" => UseCaseError::new("task_state_conflict", false),
        "task_result_digest_invalid" => UseCaseError::new("task_result_digest_invalid", false),
        "task_input_digest_invalid" => UseCaseError::new("task_input_digest_invalid", false),
        "task_input_invalid" => UseCaseError::new("task_input_invalid", false),
        "task_skill_invalid" => UseCaseError::new("task_skill_invalid", false),
        "task_mode_unsupported" => UseCaseError::new("task_mode_unsupported", false),
        "capability_snapshot_missing" => UseCaseError::new("capability_snapshot_missing", true),
        "capability_revision_exhausted" => {
            UseCaseError::new("capability_revision_exhausted", false)
        }
        "capability_admission_capacity" => UseCaseError::new("capability_admission_capacity", true),
        "capability_expiry_overflow" => UseCaseError::new("capability_expiry_overflow", false),
        "capability_workspace_capacity" => UseCaseError::new("capability_workspace_capacity", true),
        "capability_workspace_duplicate" => {
            UseCaseError::new("capability_workspace_duplicate", false)
        }
        "capability_descriptor_capacity" => {
            UseCaseError::new("capability_descriptor_capacity", true)
        }
        "capability_descriptor_duplicate" => {
            UseCaseError::new("capability_descriptor_duplicate", false)
        }
        "capability_id_invalid" => UseCaseError::new("capability_id_invalid", false),
        "capability_display_invalid" => UseCaseError::new("capability_display_invalid", false),
        "temporary_model_capability_required" => {
            UseCaseError::new("temporary_model_capability_required", false)
        }
        "task_model_source_unsupported" => {
            UseCaseError::new("task_model_source_unsupported", false)
        }
        "model_package_missing" => UseCaseError::new("model_package_missing", false),
        "model_not_exportable" => UseCaseError::new("model_not_exportable", false),
        "task_workspace_required" => UseCaseError::new("task_workspace_required", false),
        "task_workspace_not_authorized" => {
            UseCaseError::new("task_workspace_not_authorized", false)
        }
        "task_workspace_unexpected" => UseCaseError::new("task_workspace_unexpected", false),
        "task_mode_invalid" => UseCaseError::new("task_mode_invalid", false),
        "task_model_source_invalid" => UseCaseError::new("task_model_source_invalid", false),
        "task_workspace_capacity" => UseCaseError::new("task_workspace_capacity", true),
        "capability_snapshot_stale" => UseCaseError::new("capability_snapshot_stale", true),
        "capability_snapshot_expired" => UseCaseError::new("capability_snapshot_expired", true),
        "capability_workspace_not_authorized" => {
            UseCaseError::new("capability_workspace_not_authorized", false)
        }
        "desktop_remote_not_connected" => UseCaseError::new("desktop_remote_not_connected", true),
        "lease_unavailable" => UseCaseError::new("lease_unavailable", true),
        "model_binding_invalid" => UseCaseError::new("model_binding_invalid", false),
        "model_binding_stale" => UseCaseError::new("model_binding_stale", true),
        "model_credential_unavailable" => UseCaseError::new("model_credential_unavailable", false),
        "model_credential_invalid" => UseCaseError::new("model_credential_invalid", false),
        "model_credential_kind_unsupported" => {
            UseCaseError::new("model_credential_kind_unsupported", false)
        }
        "model_display_name_invalid" => UseCaseError::new("model_display_name_invalid", false),
        "model_endpoint_insecure" => UseCaseError::new("model_endpoint_insecure", false),
        "model_endpoint_invalid" => UseCaseError::new("model_endpoint_invalid", false),
        "model_endpoint_local" => UseCaseError::new("model_endpoint_local", false),
        "model_endpoint_mode_unsupported" => {
            UseCaseError::new("model_endpoint_mode_unsupported", false)
        }
        "model_endpoint_private" => UseCaseError::new("model_endpoint_private", false),
        "model_endpoint_unresolved" => UseCaseError::new("model_endpoint_unresolved", true),
        "model_image_override_unsupported" => {
            UseCaseError::new("model_image_override_unsupported", false)
        }
        "model_package_capacity" => UseCaseError::new("model_package_capacity", true),
        "model_package_field_invalid" => UseCaseError::new("model_package_field_invalid", false),
        "model_package_id_invalid" => UseCaseError::new("model_package_id_invalid", false),
        "model_package_invalid" => UseCaseError::new("model_package_invalid", false),
        "model_package_missing_field" => UseCaseError::new("model_package_missing_field", false),
        "model_package_schema_unsupported" => {
            UseCaseError::new("model_package_schema_unsupported", false)
        }
        "model_package_too_large" => UseCaseError::new("model_package_too_large", false),
        "model_package_unknown_field" => UseCaseError::new("model_package_unknown_field", false),
        "model_preset_unsupported" => UseCaseError::new("model_preset_unsupported", false),
        "model_provider_kind_unsupported" => {
            UseCaseError::new("model_provider_kind_unsupported", false)
        }
        "model_reasoning_effort_unsupported" => {
            UseCaseError::new("model_reasoning_effort_unsupported", false)
        }
        "model_route_invalid" => UseCaseError::new("model_route_invalid", false),
        "model_runtime_unavailable" => UseCaseError::new("model_runtime_unavailable", true),
        "model_secret_invalid" => UseCaseError::new("model_secret_invalid", false),
        "model_secret_too_large" => UseCaseError::new("model_secret_too_large", false),
        "model_vendor_unsupported" => UseCaseError::new("model_vendor_unsupported", false),
        "model_wire_api_mismatch" => UseCaseError::new("model_wire_api_mismatch", false),
        "model_wire_api_unsupported" => UseCaseError::new("model_wire_api_unsupported", false),
        "model_wire_name_invalid" => UseCaseError::new("model_wire_name_invalid", false),
        "controlled_session_projection_failed" => {
            UseCaseError::new("controlled_session_projection_failed", true)
        }
        "task_payload_invalid_json" => UseCaseError::new("task_payload_invalid_json", false),
        "task_payload_invalid_message" => UseCaseError::new("task_payload_invalid_message", false),
        "task_payload_invalid_type" => UseCaseError::new("task_payload_invalid_type", false),
        "task_payload_invalid_field" => UseCaseError::new("task_payload_invalid_field", false),
        "task_payload_unknown_field" => UseCaseError::new("task_payload_unknown_field", false),
        "task_payload_invalid_enum" => UseCaseError::new("task_payload_invalid_enum", false),
        "task_payload_length_mismatch" => UseCaseError::new("task_payload_length_mismatch", false),
        "task_payload_missing_field" => UseCaseError::new("task_payload_missing_field", false),
        "task_payload_unknown_message_type" => {
            UseCaseError::new("task_payload_unknown_message_type", false)
        }
        "task_payload_message_too_large" => {
            UseCaseError::new("task_payload_message_too_large", false)
        }
        "task_payload_unsupported_version" => {
            UseCaseError::new("task_payload_unsupported_version", false)
        }
        "task_payload_unsupported_capability" => {
            UseCaseError::new("task_payload_unsupported_capability", false)
        }
        "task_payload_stale_context" => UseCaseError::new("task_payload_stale_context", true),
        "task_payload_role_mismatch" => UseCaseError::new("task_payload_role_mismatch", false),
        "task_payload_replay" => UseCaseError::new("task_payload_replay", false),
        "task_status_binding_invalid" => UseCaseError::new("task_status_binding_invalid", false),
        _ => UseCaseError::new("task_request_invalid", false),
    }
}

fn payload_error(code: &str) -> UseCaseError {
    match code {
        "invalid_json" => submit_error("task_payload_invalid_json"),
        "invalid_message" => submit_error("task_payload_invalid_message"),
        "invalid_type" => submit_error("task_payload_invalid_type"),
        "invalid_field" => submit_error("task_payload_invalid_field"),
        "unknown_field" => submit_error("task_payload_unknown_field"),
        "invalid_enum" => submit_error("task_payload_invalid_enum"),
        "length_mismatch" => submit_error("task_payload_length_mismatch"),
        "missing_field" => submit_error("task_payload_missing_field"),
        "unknown_message_type" => submit_error("task_payload_unknown_message_type"),
        "message_too_large" => submit_error("task_payload_message_too_large"),
        "unsupported_version" => submit_error("task_payload_unsupported_version"),
        "unsupported_capability" => submit_error("task_payload_unsupported_capability"),
        "stale_context" => submit_error("task_payload_stale_context"),
        "role_mismatch" => submit_error("task_payload_role_mismatch"),
        "replay" => submit_error("task_payload_replay"),
        _ => UseCaseError::new("task_request_invalid", false),
    }
}

fn validated_local_text(text: &str) -> Result<&str, UseCaseError> {
    let trimmed = text.trim();
    if trimmed.is_empty() || trimmed.len() > MAX_TASK_INPUT_TEXT_BYTES {
        return Err(UseCaseError::new("task_input_invalid", false));
    }
    Ok(trimmed)
}

fn log_task_submit_boundary(stage: &str) {
    crate::features::remote_control::controller::relay_runtime::log_controlled_event(
        stage, "ready",
    );
}

struct DesktopControlledSubmitBackend {
    app: AppHandle,
}

impl DesktopControlledSubmitBackend {
    fn live_context(&self) -> Result<(LeaseContext, Vec<String>), UseCaseError> {
        self.app
            .state::<ControlledDeviceManager>()
            .live_runtime_context()
            .map_err(|error| submit_error(error.code))
    }

    fn validate_payload(
        &self,
        payload: &Value,
        lease: &LeaseContext,
        caps: &[String],
    ) -> Result<Value, UseCaseError> {
        let encoded = serde_json::to_string(payload)
            .map_err(|_| UseCaseError::new("task_request_invalid", false))?;
        let context = RuntimeValidationContext {
            lease: Some(lease.clone()),
            negotiated_capabilities: caps.to_vec(),
        };
        let validated = validate_controller_runtime_payload_json(
            &encoded,
            SenderRole::ControllerA,
            Some(&context),
        )
        .map_err(|error| payload_error(error.code))?;
        if validated.get("type").and_then(Value::as_str) != Some("task_submit") {
            return Err(UseCaseError::new("task_request_invalid", false));
        }
        Ok(validated)
    }

    fn validate_task_capability(
        &self,
        request: &RemoteTaskRequest,
        lease: &LeaseContext,
        now_b: u64,
    ) -> Result<(), UseCaseError> {
        let skill_ids = request
            .skill_ids()
            .map_err(|error| submit_error(error.code))?;
        self.app
            .state::<CapabilitySnapshotManager>()
            .validate_task_selection(
                &request.capability_revision,
                lease,
                &request.mode,
                &skill_ids,
                request.workspace_id.as_deref(),
                now_b,
            )
            .map_err(|error| submit_error(error.code))
    }
}

impl ControlledSubmissionPort for DesktopControlledSubmitBackend {
    fn validate_local_context(&self, input: &LocalControlledInput) -> Result<(), UseCaseError> {
        if !valid_remote_identifier(&input.remote_ref)
            || !valid_remote_identifier(&input.generation)
        {
            return Err(UseCaseError::new("task_session_not_found", false));
        }
        if input.code_mode != input.workspace_id.is_some() {
            return Err(UseCaseError::new("task_workspace_required", false));
        }
        let text = validated_local_text(&input.text)?;
        if input.attachments.len() > 8
            || input.skill_ids.len() > 32
            || input
                .attachments
                .iter()
                .any(|attachment| !valid_remote_identifier(&attachment.handle_id))
        {
            return Err(UseCaseError::new("task_request_invalid", false));
        }
        let mut unique_attachments = HashSet::new();
        let mut unique_skills = HashSet::new();
        if input
            .attachments
            .iter()
            .any(|attachment| !unique_attachments.insert(attachment.handle_id.as_str()))
            || input.skill_ids.iter().any(|skill_id| {
                !valid_remote_identifier(skill_id) || !unique_skills.insert(skill_id.as_str())
            })
        {
            return Err(UseCaseError::new("task_request_invalid", false));
        }
        let attachment_ids = input
            .attachments
            .iter()
            .map(|attachment| attachment.handle_id.clone())
            .collect::<Vec<_>>();
        let (lease, caps) = self.live_context()?;
        if lease.remote_control_generation != input.generation {
            return Err(UseCaseError::new("task_session_generation_stale", true));
        }
        // Exercise the frozen B task frame grammar before any call that could
        // create a Session. Real IDs/revision are generated after mapping.
        let preflight = serde_json::json!({
            "v": 1,
            "type": "task_submit",
            "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id,
            "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch,
            "remote_control_generation": lease.remote_control_generation,
            "request_id": "local-preflight",
            "task_id": "local-preflight",
            "client_request_id": "local-preflight",
            "session_id": input.expected_session_id,
            "capability_revision": "1",
            "model_source": TASK_MODEL_SOURCE_CONTROLLED,
            "mode": if input.code_mode { TASK_MODE_CODE } else { TASK_MODE_CHAT },
            "input_digest": remote_task_input_digest(text, &attachment_ids, &input.skill_ids),
            "input": {
                "text": text,
                "attachment_ids": attachment_ids,
                "skill_ids": input.skill_ids,
            },
        });
        let mut preflight = preflight;
        if let Some(workspace_id) = input.workspace_id.as_deref() {
            preflight["input"]["workspace_id"] = serde_json::json!(workspace_id);
        }
        self.validate_payload(&preflight, &lease, &caps)?;
        let runtime = self
            .app
            .state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>();
        let service = runtime.service();
        let (trusted_ref, lane, workspace_id) =
            super::desktop_remote_task::trusted_local_remote_binding(
                &service,
                &runtime.authorizations(),
                &input.expected_session_id,
                &input.generation,
            )
            .map_err(|code| submit_error(&code))?;
        if trusted_ref != input.remote_ref
            || (lane == crate::features::remote_control::controller::task::RemoteSessionLane::Code)
                != input.code_mode
            || workspace_id != input.workspace_id
        {
            return Err(UseCaseError::new("task_session_not_found", false));
        }
        Ok(())
    }

    fn ensure_remote_mapping(&self, input: &RemoteControlledInput) -> Result<String, UseCaseError> {
        let (lease, _) = self.live_context()?;
        if lease.remote_control_generation != input.generation {
            return Err(UseCaseError::new("task_session_generation_stale", true));
        }
        let task = RemoteTaskRequest::from_payload(&input.payload)
            .map_err(|error| submit_error(error.code))?;
        if task.session_id != input.remote_ref {
            return Err(UseCaseError::new("task_request_invalid", false));
        }
        // Reject an expired or policy-replaced snapshot before allocating or
        // claiming a B Session. Final admission repeats this under a snapshot
        // consistency token; this preflight is only the cheap early boundary.
        self.validate_task_capability(&task, &lease, controlled_monotonic_millis())?;
        log_task_submit_boundary("task_submit_capability_validated");
        let service = self
            .app
            .state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>()
            .service();
        let prepared_session = service
            .prepare_authenticated_task_session(&task, &lease, &input.title)
            .map_err(|error| submit_error(error.code))?;
        let b_session_id = self
            .app
            .state::<ControlledDeviceManager>()
            .with_current_business_lease(&lease, || {
                service.claim_prepared_task_session(prepared_session, &lease)
            })
            .map_err(|_| UseCaseError::new("task_session_generation_stale", true))?
            .map_err(|error| submit_error(error.code))?;
        log_task_submit_boundary("task_submit_mapping_ready");
        // Claim is already linearized under this exact owner. Transient
        // disconnect afterwards preserves all accepted generation mappings;
        // terminal cleanup, not a late preflight, owns their retirement.
        super::desktop_remote_task::project_controlled_session(
            &self.app,
            &input.remote_ref,
            &b_session_id,
            &lease,
        )
        .map_err(|code| submit_error(&code))?;
        log_task_submit_boundary("task_submit_mapping_projected");
        Ok(b_session_id)
    }

    fn ensure_local_mapping(&self, input: &LocalControlledInput) -> Result<String, UseCaseError> {
        let service = self
            .app
            .state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>()
            .service();
        let (trusted_ref, lane, workspace_id) =
            super::desktop_remote_task::trusted_local_remote_binding(
                &service,
                &self
                    .app
                    .state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>()
                    .authorizations(),
                &input.expected_session_id,
                &input.generation,
            )
            .map_err(|code| submit_error(&code))?;
        if trusted_ref == input.remote_ref
            && (lane == crate::features::remote_control::controller::task::RemoteSessionLane::Code)
                == input.code_mode
            && workspace_id == input.workspace_id
        {
            Ok(input.expected_session_id.clone())
        } else {
            Err(UseCaseError::new("task_session_not_found", false))
        }
    }

    fn prepare_remote(
        &self,
        input: RemoteControlledInput,
        b_session_id: &str,
    ) -> Result<PreparedControlledTask, UseCaseError> {
        let (lease, caps) = self.live_context()?;
        if lease.remote_control_generation != input.generation {
            return Err(UseCaseError::new("task_session_generation_stale", true));
        }
        let mut validated = self.validate_payload(&input.payload, &lease, &caps)?;
        validated["session_id"] = Value::String(b_session_id.to_owned());
        let request = RemoteTaskRequest::from_payload(&validated)
            .map_err(|error| submit_error(error.code))?;
        self.validate_task_capability(&request, &lease, controlled_monotonic_millis())?;
        log_task_submit_boundary("task_submit_prepared");
        Ok(PreparedControlledTask {
            remote_ref: input.remote_ref,
            b_session_id: b_session_id.to_owned(),
            task_id: request.task_id,
            client_request_id: request.client_request_id,
            input_digest: request.input_digest,
            payload: validated,
            local_attachments: None,
        })
    }

    fn prepare_local(
        &self,
        input: LocalControlledInput,
        b_session_id: &str,
    ) -> Result<PreparedControlledTask, UseCaseError> {
        let (lease, caps) = self.live_context()?;
        if lease.remote_control_generation != input.generation {
            return Err(UseCaseError::new("task_session_generation_stale", true));
        }
        let now_b = controlled_monotonic_millis();
        let capabilities = self.app.state::<CapabilitySnapshotManager>();
        let snapshot = capabilities
            .current_for_context(&lease, now_b)
            .map_err(|error| submit_error(error.code))?;
        let revision_payload = capabilities.wire_capability_snapshot(&snapshot, "local-revision");
        let revision = revision_payload
            .get("capability_revision")
            .and_then(Value::as_str)
            .ok_or_else(|| UseCaseError::new("capability_snapshot_missing", true))?;
        let unique = uuid::Uuid::new_v4().simple().to_string();
        let text = input.text.trim();
        let attachment_ids = input
            .attachments
            .iter()
            .map(|attachment| attachment.handle_id.clone())
            .collect::<Vec<_>>();
        let mode = if input.code_mode {
            TASK_MODE_CODE
        } else {
            TASK_MODE_CHAT
        };
        capabilities
            .validate_skills_for_mode(&lease, mode, &input.skill_ids, now_b)
            .map_err(|error| submit_error(error.code))?;
        let payload = serde_json::json!({
            "v": 1,
            "type": "task_submit",
            "connection_id": lease.connection_id,
            "controller_lease_id": lease.controller_lease_id,
            "lease_epoch": lease.lease_epoch,
            "relay_epoch": lease.relay_epoch,
            "remote_control_generation": lease.remote_control_generation,
            "request_id": format!("local-request-{unique}"),
            "task_id": format!("local-task-{unique}"),
            "client_request_id": format!("local-client-{unique}"),
            "session_id": b_session_id,
            "capability_revision": revision,
            "model_source": TASK_MODEL_SOURCE_CONTROLLED,
            "mode": mode,
            "input_digest": remote_task_input_digest(text, &attachment_ids, &input.skill_ids),
            "input": {
                "text": text,
                "attachment_ids": attachment_ids,
                "skill_ids": input.skill_ids,
            },
        });
        let mut payload = payload;
        if let Some(workspace_id) = input.workspace_id.as_deref() {
            capabilities
                .workspace_for_task(revision, &lease, workspace_id, now_b)
                .map_err(|error| submit_error(error.code))?;
            payload["input"]["workspace_id"] = serde_json::json!(workspace_id);
        }
        let validated = self.validate_payload(&payload, &lease, &caps)?;
        let request = RemoteTaskRequest::from_payload(&validated)
            .map_err(|error| submit_error(error.code))?;
        Ok(PreparedControlledTask {
            remote_ref: input.remote_ref,
            b_session_id: b_session_id.to_owned(),
            task_id: request.task_id,
            client_request_id: request.client_request_id,
            input_digest: request.input_digest,
            payload: validated,
            local_attachments: Some(input.attachments),
        })
    }

    fn admit_and_dispatch(&self, prepared: PreparedControlledTask) -> UseCaseFuture<'_, Value> {
        Box::pin(async move {
            log_task_submit_boundary("controlled_submission_admission_entered");
            dispatch_prepared_attachment_origin(
                prepared,
                |prepared| {
                    super::desktop_remote_task::submit_prepared_bound_task(
                        &self.app,
                        &prepared.b_session_id,
                        &prepared.remote_ref,
                        &prepared.payload,
                    )
                },
                |prepared, attachments| {
                    super::desktop_remote_task::submit_prepared_local_bound_task(
                        &self.app,
                        &prepared.b_session_id,
                        &prepared.remote_ref,
                        &prepared.payload,
                        attachments,
                    )
                },
            )
            .map_err(|code| submit_error(&code))
        })
    }
}

/// Preserve the authoritative input origin through final admission. `None`
/// means wire upload receipts; `Some`, including an empty vector, is B-local.
fn dispatch_prepared_attachment_origin<T>(
    mut prepared: PreparedControlledTask,
    remote: impl FnOnce(&PreparedControlledTask) -> Result<T, String>,
    local: impl FnOnce(
        &PreparedControlledTask,
        Vec<crate::features::files::file_ingest::PublishedAttachment>,
    ) -> Result<T, String>,
) -> Result<T, String> {
    match prepared.local_attachments.take() {
        None => remote(&prepared),
        Some(attachments) => local(&prepared, attachments),
    }
}

/// Finish the A-side preflight before a port is allowed to create a mapping.
/// The frozen DEV-13 request parser checks mode/workspace and digest shape in
/// addition to the wire grammar; the original IDs and digest stay immutable.
async fn submit_prevalidated_remote<P: ControlledSubmissionPort>(
    port: P,
    mut input: RemoteControlledInput,
    validated: Value,
) -> Result<ControlledSubmissionResult, UseCaseError> {
    let task = RemoteTaskRequest::from_payload(&validated)
        .map_err(|error| UseCaseError::new(error.code, false))?;
    if task.task_id != input.task_id
        || task.client_request_id != input.client_request_id
        || task.input_digest != input.input_digest
        || task.session_id != input.remote_ref
        || (task.mode == TASK_MODE_CODE) != input.code_mode
    {
        return Err(UseCaseError::new("task_request_invalid", false));
    }
    log_task_submit_boundary("task_submit_identity_validated");
    input.payload = validated;
    ControlledSubmission::new(port)
        .submit(ControlledInput::Remote(input))
        .await
}

impl ControlledExecutionPort for DesktopControlledExecutionAdapter {
    type Request = RemoteControlledInput;
    type Response = ControlledSubmissionResult;

    fn execute_controlled(&self, request: Self::Request) -> RemotePortFuture<'_, Self::Response> {
        Box::pin(async move {
            let backend = DesktopControlledSubmitBackend {
                app: self.app.clone(),
            };
            // A malformed frame must not allocate a B session or choose its
            // mode. This is protocol validation, not capability preparation;
            // the latter runs only after the idempotent mapping is established.
            let (lease, caps) = backend
                .live_context()
                .map_err(|error| RemotePortError::new(error.code(), error.is_retryable()))?;
            if lease.remote_control_generation != request.generation {
                return Err(RemotePortError::new("task_session_generation_stale", true));
            }
            log_task_submit_boundary("task_submit_context_validated");
            let validated = backend
                .validate_payload(&request.payload, &lease, &caps)
                .map_err(|error| RemotePortError::new(error.code(), error.is_retryable()))?;
            log_task_submit_boundary("task_submit_payload_validated");
            submit_prevalidated_remote(backend, request, validated)
                .await
                .map_err(|error| RemotePortError::new(error.code(), error.is_retryable()))
        })
    }
}

impl DesktopControlledBusiness {
    async fn handle_remote_permission_decision(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        body: &Value,
    ) -> Result<Value, String> {
        let controlled = self.app.state::<ControlledDeviceManager>();
        let (owner, request_id, decision) = controlled
            .with_current_business_lease(lease, || {
                let field = |key: &str| -> Result<&str, String> {
                    body.get(key)
                        .and_then(Value::as_str)
                        .ok_or_else(|| "remote_action_invalid".to_owned())
                };
                let task_id = field("task_id")?;
                let request_id = field("permission_request_id")?;
                let decision = PermissionDecision::parse(field("decision")?)
                    .ok_or_else(|| "permission_decision_invalid".to_owned())?;
                let tasks = self
                    .app
                    .state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>();
                let session_id = tasks
                    .service()
                    .session_for(session_ref, &lease.remote_control_generation)
                    .ok_or_else(|| "task_session_binding_mismatch".to_owned())?;
                let record = self
                    .app
                    .state::<Arc<RemoteTaskLedger>>()
                    .record(task_id)
                    .map_err(|error| error.code.to_owned())?;
                if record.session_id != session_id
                    || record.lease != TaskLeaseBinding::from_context(lease)
                    || record.is_terminal()
                {
                    return Err("task_session_binding_mismatch".to_owned());
                }
                let authority = self.app.state::<Arc<PermissionAuthority>>();
                let outcome = authority
                    .decide(
                        DecisionAuthority::ControllerA,
                        lease,
                        task_id,
                        request_id,
                        decision,
                        authority.now_ms(),
                    )
                    .map_err(|error| error.code.to_owned())?;
                commit_effective_permission_allow(&authority, request_id, &outcome)?;
                Ok((
                    RemotePermissionOwner {
                        session_id,
                        task_id: task_id.to_owned(),
                        lease: lease.clone(),
                    },
                    request_id.to_owned(),
                    if matches!(outcome, DecisionOutcome::Expired) {
                        PermissionDecision::Deny
                    } else {
                        decision
                    },
                ))
            })
            .map_err(|error| error.code.to_owned())??;

        super::desktop_remote_permission::respond_remote_permission_via_acp(
            &self.app.state::<AcpPool>(),
            &owner,
            &request_id,
            decision,
        )
        .await?;
        controlled
            .with_current_business_lease(lease, || {
                let record = self
                    .app
                    .state::<Arc<RemoteTaskLedger>>()
                    .record(&owner.task_id)
                    .map_err(|error| error.code.to_owned())?;
                if record.session_id != owner.session_id
                    || record.lease != TaskLeaseBinding::from_context(lease)
                    || !matches!(
                        record.state,
                        RemoteTaskState::Running | RemoteTaskState::WaitingPermission
                    )
                {
                    return Err("task_session_binding_mismatch".to_owned());
                }
                Ok(())
            })
            .map_err(|error| error.code.to_owned())??;
        let view = self
            .app
            .state::<Arc<PermissionAuthority>>()
            .view(&request_id)
            .map_err(|error| error.code.to_owned())?;
        Ok(json!({"state": view.state}))
    }

    /// Resolve the A reference only through the current authenticated B
    /// generation's authoritative map. A caller cannot select an arbitrary
    /// local journal by supplying a different reference.
    fn authoritative_journal_session(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
    ) -> Result<String, String> {
        let (current, _) = self
            .app
            .state::<ControlledDeviceManager>()
            .live_runtime_context()
            .map_err(|_| "journal_lease_stale".to_owned())?;
        if &current != lease {
            return Err("journal_lease_stale".to_owned());
        }
        let runtime = self
            .app
            .state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>();
        runtime
            .service()
            .session_for(session_ref, &lease.remote_control_generation)
            .ok_or_else(|| "journal_session_mapping_missing".to_owned())
    }

    fn handle_remote_action(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        operation: &str,
        body: &Value,
    ) -> Result<Value, String> {
        let controlled = self.app.state::<ControlledDeviceManager>();
        controlled
            .with_current_business_lease(lease, || {
                let tasks = self
                    .app
                    .state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>();
                let session_id = tasks
                    .service()
                    .session_for(session_ref, &lease.remote_control_generation)
                    .ok_or_else(|| "task_session_binding_mismatch".to_owned())?;
                let field = |key: &str| -> Result<&str, String> {
                    body.get(key)
                        .and_then(Value::as_str)
                        .ok_or_else(|| "remote_action_invalid".to_owned())
                };
                let number = |key: &str| -> Result<u64, String> {
                    field(key)?.parse::<u64>().map_err(|_| "remote_action_invalid".to_owned())
                };
                let task_id = body.get("task_id").and_then(Value::as_str);
                let require_task = |allow_terminal: bool| -> Result<(), String> {
                    let record = self
                        .app
                        .state::<Arc<RemoteTaskLedger>>()
                        .record(task_id.ok_or_else(|| "remote_action_invalid".to_owned())?)
                        .map_err(|error| error.code.to_owned())?;
                    if record.session_id != session_id
                        || record.lease != TaskLeaseBinding::from_context(lease)
                        || (!allow_terminal && record.is_terminal())
                    {
                        return Err("task_session_binding_mismatch".to_owned());
                    }
                    Ok(())
                };
                let now = controlled_monotonic_millis();
                match operation {
                    "permission_list" => {
                        require_task(false)?;
                        let views = self.app.state::<Arc<PermissionAuthority>>()
                            .pending_for_task(field("task_id")?);
                        if views.len() > 8 { return Err("permission_capacity".to_owned()); }
                        let permissions: Vec<Value> = views.into_iter().map(|view| json!({
                            "permission_request_id":view.permission_request_id,"task_id":view.task_id,
                            "operation_type":view.operation_type,"risk_level":view.risk_level,
                            "target_scope":view.target_scope,"preview":view.preview,
                            "expires_at_ms":view.expires_at_ms.to_string(),"state":view.state,
                            "decision":view.decision,"dispatch_state":view.dispatch_state,
                        })).collect();
                        Ok(json!({"permissions": permissions}))
                    }
                    "permission_decide" => {
                        Err("remote_action_async_required".to_owned())
                    }
                    "attachment_begin" => {
                        let runtime = self.app.state::<Arc<super::desktop_remote_attachment::DesktopRemoteAttachmentRuntime>>();
                        let context = TransferContext::new(lease, &session_id, field("task_id")?, TransferDirection::Upload)
                            .map_err(|error| error.code.to_owned())?;
                        let metadata = TransferMetadata {
                            file_name: field("file_name")?.to_owned(), mime_type: field("mime_type")?.to_owned(),
                            total_bytes: number("total_bytes")?,
                            sha256: field("sha256")?.to_owned(),
                        };
                        let begun = runtime.uploads().begin(context, metadata, now)?;
                        Ok(json!({"transfer_id": begun.handle_id, "chunk_bytes": begun.chunk_bytes.to_string(),
                            "total_bytes": begun.total_bytes.to_string()}))
                    }
                    "attachment_chunk" | "attachment_abort" => {
                        let runtime = self.app.state::<Arc<super::desktop_remote_attachment::DesktopRemoteAttachmentRuntime>>();
                        runtime.uploads().require_upload_binding(
                            field("transfer_id")?, lease, &session_id, field("task_id")?,
                        )?;
                        if operation == "attachment_abort" {
                            return Ok(json!({"aborted": runtime.uploads().abort(field("transfer_id")?)}));
                        }
                        let bytes = decode_upload_chunk(
                            field("chunk_b64")?, number("chunk_bytes")?, runtime.uploads().chunk_bytes(),
                        )?;
                        let progress = runtime.uploads().accept_chunk(
                            field("transfer_id")?, number("chunk_index")?,
                            &bytes, now,
                        )?;
                        Ok(match progress {
                            UploadProgress::Accepted { received_bytes, .. } => json!({"state":"accepted","received_bytes":received_bytes.to_string()}),
                            UploadProgress::Duplicate { received_bytes } => json!({"state":"duplicate","received_bytes":received_bytes.to_string()}),
                            UploadProgress::Completed { sha256, bytes } => json!({"state":"completed","received_bytes":bytes.to_string(),"sha256":sha256}),
                        })
                    }
                    "artifact_index" | "artifact_preview" | "artifact_download_begin"
                    | "artifact_download_chunk" | "artifact_download_abort" => {
                        let runtime = self.app.state::<Arc<super::desktop_remote_artifact::DesktopRemoteArtifactRuntime>>();
                        match operation {
                            "artifact_index" => {
                                let artifacts: Vec<Value> = runtime.artifact_index(lease, &session_id)?.into_iter()
                                    .map(|item| json!({"artifact_id":item.artifact_id,
                                        "task_id":item.task_id,
                                        "display_name":item.display_name,"mime_type":item.mime_type,
                                        "total_bytes":item.total_bytes.to_string(),"sha256":item.sha256,
                                        "preview_kind":item.preview_kind})).collect();
                                Ok(json!({"artifacts":artifacts}))
                            }
                            "artifact_preview" => {
                                let preview = runtime.artifact_preview(lease, &session_id, field("artifact_id")?)?;
                                let mut body = json!({"artifact_id":preview.artifact_id,
                                    "preview_kind":preview.preview_kind,"truncated":preview.truncated});
                                if let Some(text) = preview.text { body["text"] = json!(text); }
                                Ok(body)
                            }
                            "artifact_download_begin" => {
                                let begun = runtime.artifact_download_begin(lease, &session_id,
                                    field("task_id")?, field("artifact_id")?, now)?;
                                Ok(json!({"transfer_id":begun.handle_id,"chunk_bytes":begun.chunk_bytes.to_string(),
                                    "total_bytes":begun.total_bytes.to_string()}))
                            }
                            "artifact_download_chunk" => {
                                let chunk = runtime.artifact_download_chunk(lease, &session_id, field("task_id")?,
                                    field("artifact_id")?, field("transfer_id")?, number("chunk_index")?, now)?
                                    .ok_or_else(|| "artifact_unavailable".to_owned())?;
                                Ok(json!({"chunk_index":chunk.chunk_index.to_string(),
                                    "chunk_b64":base64::engine::general_purpose::STANDARD.encode(&chunk.bytes),
                                    "chunk_bytes":chunk.bytes.len().to_string(), "received_bytes":chunk.received_bytes.to_string(),
                                    "next_chunk":chunk.next_chunk.to_string(),"completed":chunk.completed.is_some()}))
                            }
                            _ => {
                                Ok(json!({"aborted":runtime.artifact_download_abort(lease, &session_id,
                                    field("task_id")?, field("artifact_id")?, field("transfer_id")?)?}))
                            }
                        }
                    }
                    _ => Err("remote_action_invalid".to_owned()),
                }
            })
            .map_err(|error| error.code.to_owned())?
    }
}

/// B sends its actual Session identity only inside the authenticated encrypted
/// capability response; A's opaque wire ref remains a distinct field.
fn bind_capability_session(
    mut payload: Value,
    remote_ref: &str,
    controlled_session_id: &str,
) -> Result<Value, String> {
    let object = payload
        .as_object_mut()
        .ok_or_else(|| "capability_snapshot_invalid".to_owned())?;
    object.insert("session_id".into(), Value::String(remote_ref.to_owned()));
    object.insert(
        "controlled_session_id".into(),
        Value::String(controlled_session_id.to_owned()),
    );
    Ok(payload)
}

#[async_trait::async_trait]
impl ControlledBusinessPort for DesktopControlledBusiness {
    async fn remote_action(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        operation: &str,
        body: &Value,
    ) -> Result<Value, String> {
        if operation == "permission_decide" {
            self.handle_remote_permission_decision(lease, session_ref, body)
                .await
        } else {
            self.handle_remote_action(lease, session_ref, operation, body)
        }
    }
    async fn capability_snapshot(
        &self,
        lease: &LeaseContext,
        request_id: &str,
        session_id: &str,
        known_revision: u64,
    ) -> Result<Value, String> {
        let authoritative_id = super::desktop_remote_task::ensure_unclaimed_controlled_session(
            &self.app, session_id, "", lease,
        )?;
        let capabilities = self.app.state::<CapabilitySnapshotManager>();
        let now_b =
            crate::features::remote_control::controller::relay_runtime::controlled_monotonic_millis(
            );
        // 只复用刚生成、且比 A 已知 revision 更新的快照：这能吸收紧邻的重复
        // 请求，也不会把临近过期的快照误报成完整 30 秒有效期。
        if let Ok(current) = capabilities.current_for_context(lease, now_b) {
            let payload = capabilities.wire_capability_snapshot(&current, request_id);
            let revision = payload["capability_revision"]
                .as_str()
                .and_then(|value| value.parse::<u64>().ok())
                .unwrap_or(0);
            let remaining_ms = payload["expires_at_ms"]
                .as_str()
                .and_then(|value| value.parse::<u64>().ok())
                .unwrap_or(0)
                .saturating_sub(now_b);
            if revision > known_revision && remaining_ms >= 29_000 {
                super::desktop_remote_task::project_controlled_session(
                    &self.app,
                    session_id,
                    &authoritative_id,
                    lease,
                )?;
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "capability_snapshot_ready",
                    "cached",
                );
                return bind_capability_session(payload, session_id, &authoritative_id);
            }
        }
        let snapshot = match super::desktop_remote_capabilities::build_capability_snapshot(
            &self.app,
        )
        .await
        {
            Ok(snapshot) => snapshot,
            Err(error) => {
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "capability_snapshot_failed",
                    &error,
                );
                return Err(error);
            }
        };
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "capability_snapshot_ready",
            "fresh",
        );
        super::desktop_remote_task::project_controlled_session(
            &self.app,
            session_id,
            &authoritative_id,
            lease,
        )?;
        bind_capability_session(
            capabilities.wire_capability_snapshot(&snapshot, request_id),
            session_id,
            &authoritative_id,
        )
    }

    async fn submit_task(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        payload: &Value,
    ) -> Result<Value, String> {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "task_submit_business_entered",
            "validating",
        );
        let field = |name: &str| {
            payload
                .get(name)
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned()
        };
        let request = RemoteControlledInput {
            remote_ref: session_ref.to_owned(),
            generation: lease.remote_control_generation.clone(),
            title: String::new(),
            code_mode: payload.get("mode").and_then(Value::as_str) == Some("code"),
            task_id: field("task_id"),
            client_request_id: field("client_request_id"),
            input_digest: field("input_digest"),
            payload: payload.clone(),
        };
        let port = self
            .app
            .state::<crate::app::desktop_remote_composition::DesktopControlledExecutionPort>();
        let result = port.execute_controlled(request).await;
        match &result {
            Ok(_) => {
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "task_submit_business_accepted",
                    "admitted",
                )
            }
            Err(error) => log_controlled_port_error("task_submit_business_rejected", *error),
        }
        result
            .map(|outcome| outcome.status)
            .map_err(|error| error.code().to_owned())
    }

    async fn cancel_task(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        task_id: &str,
    ) -> Result<Option<Value>, String> {
        super::desktop_remote_task::cancel_decrypted_task(&self.app, lease, session_ref, task_id)
            .map(Some)
    }

    async fn replay_journal(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        journal_epoch: &str,
        after_sequence: u64,
    ) -> Result<Vec<(String, Value)>, String> {
        let b_session_id = self.authoritative_journal_session(lease, session_ref)?;
        let history = self
            .app
            .state::<std::sync::Arc<super::desktop_remote_journal::DesktopRemoteJournalRuntime>>();
        history
            .journal()
            .bind_generation(&lease.remote_control_generation)
            .map_err(|error| error.code.to_owned())?;
        Ok(
            match history
                .journal()
                .replay_after_for_generation(
                    &lease.remote_control_generation,
                    &b_session_id,
                    journal_epoch,
                    after_sequence,
                )
                .map_err(|error| error.code.to_owned())?
            {
                crate::features::remote_control::controller::journal::ReplayOutcome::Events(
                    events,
                ) => build_replay_frames(lease, session_ref, &events)?,
                crate::features::remote_control::controller::journal::ReplayOutcome::Gap(
                    snapshot,
                ) => {
                    vec![(
                        "session_snapshot".to_owned(),
                        build_session_snapshot_payload(
                            lease,
                            session_ref,
                            "request-replay-gap",
                            &snapshot,
                        )?,
                    )]
                }
            },
        )
    }

    async fn session_snapshot(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        request_id: &str,
    ) -> Result<Value, String> {
        let b_session_id = self.authoritative_journal_session(lease, session_ref)?;
        let history = self
            .app
            .state::<std::sync::Arc<super::desktop_remote_journal::DesktopRemoteJournalRuntime>>();
        history
            .journal()
            .bind_generation(&lease.remote_control_generation)
            .map_err(|error| error.code.to_owned())?;
        let snapshot = history
            .journal()
            .snapshot_for_generation(&lease.remote_control_generation, &b_session_id)
            .map_err(|error| error.code.to_owned())?;
        build_session_snapshot_payload(lease, session_ref, request_id, &snapshot)
    }
}

/// 一次回放最多回传的帧数：journal 保留窗口有界，但仍给回放本身一个硬上限。
const MAX_REPLAY_FRAMES: usize = 256;

fn build_replay_frames(
    lease: &LeaseContext,
    session_ref: &str,
    events: &[crate::features::remote_control::controller::journal::JournalEvent],
) -> Result<Vec<(String, Value)>, String> {
    let mut frames = Vec::with_capacity(events.len());
    for page in events.chunks(MAX_REPLAY_FRAMES) {
        for event in page {
            let payload = crate::features::remote_control::controller::relay_runtime::build_journal_event_payload(
                lease, session_ref, event,
            ).ok_or_else(|| "journal_event_not_exportable".to_owned())?;
            frames.push(("journal_event".to_owned(), payload));
        }
    }
    Ok(frames)
}

/// 构造 wire `session_snapshot`（E2E-05 缺口恢复）：快照本体 base64 + 摘要，
/// 会话身份使用 A 的引用。
fn build_session_snapshot_payload(
    lease: &LeaseContext,
    session_ref: &str,
    request_id: &str,
    snapshot: &crate::features::remote_control::controller::journal::JournalSnapshot,
) -> Result<Value, String> {
    // Remapping B's authoritative identity to A's opaque reference changes
    // the committed bytes; the digest must be recomputed over that wire body.
    let wire_snapshot = snapshot
        .remap_session(session_ref)
        .map_err(|error| error.code.to_owned())?;
    let raw =
        serde_json::to_vec(&wire_snapshot).map_err(|_| "journal_snapshot_invalid".to_owned())?;
    let digest = wire_snapshot.digest.clone();
    if digest.len() != 64 {
        return Err("journal_snapshot_invalid".to_owned());
    }
    let payload = serde_json::json!({
        "v": 1,
        "type": "session_snapshot",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": request_id,
        "session_id": session_ref,
        "journal_epoch": wire_snapshot.epoch,
        "base_sequence": wire_snapshot.sequence.to_string(),
        "snapshot_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
        "snapshot_bytes": raw.len().to_string(),
        "sha256": digest,
    });
    // The encrypted lane validates plaintext JSON at 512 KiB. Check the
    // complete envelope here, including base64 and all context fields, so a
    // retained journal snapshot is never silently unsendable.
    let wire_len = serde_json::to_vec(&payload)
        .map_err(|_| "journal_snapshot_invalid".to_owned())?
        .len();
    if wire_len > crate::features::remote_control::controller::protocol::MAX_DECRYPTED_PAYLOAD_BYTES
    {
        return Err("journal_snapshot_too_large".to_owned());
    }
    Ok(payload)
}

#[cfg(test)]
mod dev19_replay_tests {
    use super::*;
    use crate::features::remote_control::controller::journal::{
        JournalKind, JournalSnapshot, RemoteJournal,
    };
    use serde_json::json;

    #[test]
    fn prepared_remote_input_consumes_real_completed_upload_and_local_empty_stays_local() {
        use crate::features::remote_control::controller::transfer::{
            core::TransferRegistry, limits::TransferLimits, staging::SessionIsolationStaging,
            upload::AttachmentUploads,
        };
        use sha2::{Digest, Sha256};
        let temp = tempfile::tempdir().unwrap();
        let root = temp.path().to_path_buf();
        let staging = Arc::new(SessionIsolationStaging::new(Arc::new(move |_| {
            Ok(root.clone())
        })));
        let uploads = AttachmentUploads::new(
            Arc::new(TransferRegistry::new(TransferLimits::default()).unwrap()),
            staging,
        );
        let live = lease();
        let bytes = b"actual completed wire attachment";
        let begun = uploads
            .begin(
                TransferContext::new(&live, "session-1", "task-1", TransferDirection::Upload)
                    .unwrap(),
                TransferMetadata {
                    file_name: "wire.txt".into(),
                    mime_type: "text/plain".into(),
                    total_bytes: bytes.len() as u64,
                    sha256: Sha256::digest(bytes)
                        .iter()
                        .map(|byte| format!("{byte:02x}"))
                        .collect(),
                },
                1_000,
            )
            .unwrap();
        uploads
            .accept_chunk(&begun.handle_id, 0, bytes, 1_100)
            .unwrap();
        let prepared = |local_attachments| PreparedControlledTask {
            remote_ref: "ref-1".into(),
            b_session_id: "session-1".into(),
            task_id: "task-1".into(),
            client_request_id: "request-1".into(),
            input_digest: "digest-1".into(),
            payload: json!({"input":{"attachment_ids":[begun.handle_id]}}),
            local_attachments,
        };
        // These are the production port's actual Some/None dispatch semantics,
        // with real upload receipt ownership and real anchored published bytes.
        let attachments = dispatch_prepared_attachment_origin(
            prepared(None),
            |task| {
                let ids = task.payload["input"]["attachment_ids"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|id| id.as_str().unwrap().to_owned())
                    .collect::<Vec<_>>();
                uploads.consume_completed(&ids, &live, &task.b_session_id, &task.task_id)
            },
            |_, _| panic!("wire attachments must not enter the B-local descriptor arm"),
        )
        .unwrap();
        assert_eq!(attachments.len(), 1);
        assert_eq!(attachments[0].handle_id, begun.handle_id);
        assert_eq!(
            attachments[0]
                .read_owner
                .as_ref()
                .unwrap()
                .read_bounded(bytes.len() as u64)
                .unwrap(),
            bytes
        );
        assert!(
            uploads
                .consume_completed(&[begun.handle_id.clone()], &live, "session-1", "task-1")
                .is_err()
        );
        let local = dispatch_prepared_attachment_origin(
            prepared(Some(Vec::new())),
            |_| panic!("even empty B-local descriptors must not consume wire receipts"),
            |_, attachments| Ok(attachments),
        )
        .unwrap();
        assert!(local.is_empty());
        drop(attachments);
        uploads.cleanup_for_task("task-1");
    }

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: "generation-1".into(),
        }
    }

    #[test]
    fn replay_over_256_is_fully_paged_without_silent_truncation() {
        let journal = RemoteJournal::new("epoch-1");
        for sequence in 1..=300 {
            journal
                .append(
                    &format!("event-{sequence}"),
                    JournalKind::UserMessage,
                    "b-session",
                    None,
                    json!({"text": "safe"}),
                )
                .unwrap();
        }
        let events = match journal.replay_after_for("b-session", "epoch-1", 0).unwrap() {
            crate::features::remote_control::controller::journal::ReplayOutcome::Events(events) => {
                events
            }
            _ => panic!("window should be replayable"),
        };
        let frames = build_replay_frames(&lease(), "remote-ref", &events).unwrap();
        assert_eq!(frames.len(), 300);
        assert_eq!(frames.first().unwrap().1["event_sequence"], "1");
        assert_eq!(frames.last().unwrap().1["event_sequence"], "300");
        assert!(
            frames
                .iter()
                .all(|(_, frame)| frame["session_id"] == "remote-ref")
        );
    }

    #[test]
    fn wire_snapshot_rehashes_exact_remapped_content() {
        let journal = RemoteJournal::new("epoch-1");
        journal
            .append(
                "event-1",
                JournalKind::UserMessage,
                "b-session",
                None,
                json!({"text":"safe"}),
            )
            .unwrap();
        let original = journal.snapshot_for("b-session").unwrap();
        let wire =
            build_session_snapshot_payload(&lease(), "remote-ref", "request-1", &original).unwrap();
        let raw = base64::engine::general_purpose::STANDARD
            .decode(wire["snapshot_b64"].as_str().unwrap())
            .unwrap();
        let mapped: JournalSnapshot = serde_json::from_slice(&raw).unwrap();
        assert!(mapped.verify_digest());
        assert_eq!(mapped.session_id, "remote-ref");
        assert_eq!(mapped.events[0].session_id, "remote-ref");
        assert_ne!(mapped.digest, original.digest);
        assert_eq!(wire["sha256"], mapped.digest);
    }

    #[test]
    fn retained_snapshot_fits_plaintext_budget_and_oversize_fails_closed() {
        let journal = RemoteJournal::new("epoch-1");
        for sequence in 1..=100 {
            journal
                .append(
                    &format!("event-{sequence}"),
                    JournalKind::UserMessage,
                    "b-session",
                    None,
                    json!({"text":"x".repeat(4096)}),
                )
                .unwrap();
        }
        let retained = journal.snapshot_for("b-session").unwrap();
        let near_wire =
            build_session_snapshot_payload(&lease(), &"r".repeat(128), &"q".repeat(128), &retained)
                .unwrap();
        let near_bytes = serde_json::to_vec(&near_wire).unwrap().len();
        assert!(
            near_bytes > 300 * 1024,
            "test must approach the wire ceiling"
        );
        assert!(
            near_bytes
                <= crate::features::remote_control::controller::protocol::MAX_DECRYPTED_PAYLOAD_BYTES
        );

        let mut too_large = retained.clone();
        let template = too_large.events.last().unwrap().clone();
        too_large.events.clear();
        for sequence in 1..=150 {
            let mut event = template.clone();
            event.event_id = format!("oversized-{sequence}");
            event.sequence = sequence;
            too_large.events.push(event);
        }
        too_large.sequence = 150;
        too_large.digest = too_large.compute_digest();
        assert!(too_large.verify_digest());
        assert_eq!(
            build_session_snapshot_payload(&lease(), "remote-ref", "request-1", &too_large)
                .unwrap_err(),
            "journal_snapshot_too_large"
        );
    }
}

/// B 端是否仍持有活租约（供 worker 在派发前自检；当前由设备所有者状态判断）。
#[allow(dead_code)]
pub(crate) fn controlled_is_live(app: &AppHandle) -> bool {
    app.state::<ControlledDeviceManager>()
        .live_runtime_context()
        .is_ok()
}

#[cfg(test)]
mod submit_boundary_tests {
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};

    use parking_lot::Mutex;
    use serde_json::{Value, json};

    use super::{
        ControlledSubmissionPort, LocalControlledInput, MAX_TASK_INPUT_TEXT_BYTES,
        PreparedControlledTask, RemoteControlledInput, SenderRole, UseCaseError, UseCaseFuture,
        payload_error, submit_error, submit_prevalidated_remote, validated_local_text,
    };
    use crate::features::remote_control::controller::controlling_session::task_input_digest;
    use crate::features::remote_control::controller::protocol::validate_controller_payload_json;

    #[test]
    fn same_allow_retries_and_concurrent_dispatch_are_idempotent() {
        use super::*;
        use crate::features::remote_control::controller::permission::PermissionRequest;
        let authority = Arc::new(PermissionAuthority::with_clock(Arc::new(|| 9_001_000)));
        let lease = LeaseContext {
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: "generation-1".into(),
        };
        let id = authority
            .request(
                &lease,
                "task-1",
                PermissionRequest {
                    operation_type: "shell_command".into(),
                    risk_level: "high".into(),
                    target_scope: "workspace-1".into(),
                    preview: "command".into(),
                    expires_at_ms: authority.now_ms() + 60_000,
                },
                authority.now_ms(),
            )
            .unwrap()
            .permission_request_id;
        // Both callers observe allow before either crosses the dispatch CAS.
        let barrier = Arc::new(std::sync::Barrier::new(2));
        let handles: Vec<_> = (0..2)
            .map(|_| {
                let authority = Arc::clone(&authority);
                let barrier = Arc::clone(&barrier);
                let lease = lease.clone();
                let id = id.clone();
                std::thread::spawn(move || {
                    let outcome = authority
                        .decide(
                            DecisionAuthority::ControllerA,
                            &lease,
                            "task-1",
                            &id,
                            PermissionDecision::Allow,
                            authority.now_ms(),
                        )
                        .unwrap();
                    barrier.wait();
                    commit_effective_permission_allow(&authority, &id, &outcome)
                })
            })
            .collect();
        for handle in handles {
            assert_eq!(handle.join().unwrap(), Ok(()));
        }
        let retry = authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease,
                "task-1",
                &id,
                PermissionDecision::Allow,
                authority.now_ms(),
            )
            .unwrap();
        assert_eq!(
            retry,
            DecisionOutcome::Replayed {
                decision: PermissionDecision::Allow,
                dispatch_state: DispatchState::Dispatched,
            }
        );
        assert_eq!(
            commit_effective_permission_allow(&authority, &id, &retry),
            Ok(())
        );
        assert_eq!(
            authority.commit_dispatch(&id, authority.now_ms()).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_already_dispatched"
            }
        );
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease,
                    "task-1",
                    &id,
                    PermissionDecision::Deny,
                    authority.now_ms()
                )
                .unwrap_err()
                .code,
            "permission_already_decided"
        );
        assert_eq!(authority.view(&id).unwrap().dispatch_state, "dispatched");
    }

    #[derive(Default)]
    struct SubmissionTrace {
        mapped: AtomicUsize,
        admitted: AtomicUsize,
        identity: Mutex<Option<(String, String, String)>>,
    }

    struct RecordingPort(Arc<SubmissionTrace>);

    impl ControlledSubmissionPort for RecordingPort {
        fn validate_local_context(&self, _: &LocalControlledInput) -> Result<(), UseCaseError> {
            panic!("remote preflight must not validate a local input")
        }

        fn ensure_remote_mapping(&self, _: &RemoteControlledInput) -> Result<String, UseCaseError> {
            self.0.mapped.fetch_add(1, Ordering::SeqCst);
            Ok("b-session".to_owned())
        }

        fn ensure_local_mapping(&self, _: &LocalControlledInput) -> Result<String, UseCaseError> {
            panic!("remote preflight must not map a local input")
        }

        fn prepare_remote(
            &self,
            input: RemoteControlledInput,
            b_session_id: &str,
        ) -> Result<PreparedControlledTask, UseCaseError> {
            let mut payload = input.payload;
            payload["session_id"] = json!(b_session_id);
            Ok(PreparedControlledTask {
                remote_ref: input.remote_ref,
                b_session_id: b_session_id.to_owned(),
                task_id: input.task_id,
                client_request_id: input.client_request_id,
                input_digest: input.input_digest,
                payload,
                local_attachments: None,
            })
        }

        fn prepare_local(
            &self,
            _: LocalControlledInput,
            _: &str,
        ) -> Result<PreparedControlledTask, UseCaseError> {
            panic!("remote preflight must not prepare a local input")
        }

        fn admit_and_dispatch(&self, prepared: PreparedControlledTask) -> UseCaseFuture<'_, Value> {
            self.0.admitted.fetch_add(1, Ordering::SeqCst);
            *self.0.identity.lock() = Some((
                prepared.task_id.clone(),
                prepared.client_request_id.clone(),
                prepared.input_digest.clone(),
            ));
            Box::pin(async move {
                Ok(json!({
                    "task_id": prepared.task_id,
                    "session_id": prepared.b_session_id,
                    "task_status": "admitted",
                }))
            })
        }
    }

    fn remote_frame(mode: &str) -> Value {
        json!({
            "v": 1, "type": "task_submit",
            "connection_id": "connection-1", "controller_lease_id": "lease-1",
            "lease_epoch": "epoch-1", "relay_epoch": "relay-1",
            "remote_control_generation": "generation-1",
            "request_id": "request-1", "task_id": "task-a",
            "client_request_id": "client-a", "session_id": "opaque-ref",
            "capability_revision": "1", "model_source": "controlled_device",
            "mode": mode, "input_digest": task_input_digest("hello"),
            "input": { "text": "hello", "attachment_ids": [], "skill_ids": [] },
        })
    }

    fn remote_input(frame: &Value) -> RemoteControlledInput {
        RemoteControlledInput {
            remote_ref: "opaque-ref".to_owned(),
            generation: "generation-1".to_owned(),
            title: String::new(),
            code_mode: frame["mode"] == "code",
            task_id: "task-a".to_owned(),
            client_request_id: "client-a".to_owned(),
            input_digest: frame["input_digest"].as_str().unwrap().to_owned(),
            payload: frame.clone(),
        }
    }

    #[tokio::test]
    async fn a_mode_shape_preflight_rejects_before_mapping_or_admission() {
        for (frame, code) in [
            (remote_frame("code"), "task_workspace_required"),
            (
                {
                    let mut frame = remote_frame("chat");
                    frame["input"]["workspace_id"] = json!("workspace-1");
                    frame
                },
                "task_workspace_unexpected",
            ),
        ] {
            let trace = Arc::new(SubmissionTrace::default());
            let error = submit_prevalidated_remote(
                RecordingPort(Arc::clone(&trace)),
                remote_input(&frame),
                frame,
            )
            .await
            .unwrap_err();
            assert_eq!(error.code(), code);
            assert!(!error.is_retryable());
            assert_eq!(trace.mapped.load(Ordering::SeqCst), 0);
            assert_eq!(trace.admitted.load(Ordering::SeqCst), 0);
        }
    }

    #[tokio::test]
    async fn a_shape_preflight_preserves_authenticated_ids_and_digest() {
        let frame = remote_frame("chat");
        let digest = frame["input_digest"].as_str().unwrap().to_owned();
        let trace = Arc::new(SubmissionTrace::default());
        let result = submit_prevalidated_remote(
            RecordingPort(Arc::clone(&trace)),
            remote_input(&frame),
            frame,
        )
        .await
        .unwrap();
        assert_eq!(result.b_session_id, "b-session");
        assert_eq!(result.task_id, "task-a");
        assert_eq!(trace.mapped.load(Ordering::SeqCst), 1);
        assert_eq!(trace.admitted.load(Ordering::SeqCst), 1);
        assert_eq!(
            *trace.identity.lock(),
            Some(("task-a".to_owned(), "client-a".to_owned(), digest))
        );
    }

    #[test]
    fn shared_admission_errors_keep_retry_and_conflict_semantics() {
        let busy = submit_error("task_session_busy");
        assert_eq!(busy.code(), "task_session_busy");
        assert!(busy.is_retryable());
        let conflict = submit_error("task_request_conflict");
        assert_eq!(conflict.code(), "task_request_conflict");
        assert!(!conflict.is_retryable());
        assert_eq!(
            submit_error("temporary_model_capability_required").code(),
            "temporary_model_capability_required"
        );
        assert_eq!(
            submit_error("controlled_session_projection_failed").code(),
            "controlled_session_projection_failed"
        );
        for (code, retryable) in [
            ("task_binding_stale", true),
            ("task_ledger_capacity", true),
            ("task_ledger_persist_failed", true),
            ("task_session_backend_bind_failed", true),
            ("task_status_binding_invalid", false),
            ("task_workspace_unexpected", false),
            ("task_workspace_not_authorized", false),
            ("task_session_binding_mismatch", false),
            ("task_mode_invalid", false),
            ("task_model_source_invalid", false),
            ("capability_workspace_capacity", true),
            ("model_not_exportable", false),
        ] {
            let error = submit_error(code);
            assert_eq!(error.code(), code);
            assert_eq!(error.is_retryable(), retryable, "{code}");
        }
        for raw in [
            "invalid_enum",
            "invalid_field",
            "invalid_json",
            "invalid_message",
            "invalid_type",
            "length_mismatch",
            "message_too_large",
            "missing_field",
            "replay",
            "role_mismatch",
            "stale_context",
            "unknown_field",
            "unknown_message_type",
            "unsupported_capability",
            "unsupported_version",
        ] {
            let expected = format!("task_payload_{raw}");
            assert_eq!(payload_error(raw).code(), expected);
            assert_eq!(submit_error(&expected).code(), expected);
        }
        assert!(payload_error("stale_context").is_retryable());
        assert_eq!(
            submit_error("task_payload_invalid_enum").code(),
            "task_payload_invalid_enum"
        );
        assert_eq!(
            submit_error("untrusted detail").code(),
            "task_request_invalid"
        );
    }

    #[test]
    fn b_text_preflight_tracks_the_frozen_task_protocol_byte_budget() {
        for (text, expected) in [
            ("x".repeat(MAX_TASK_INPUT_TEXT_BYTES), true),
            ("x".repeat(MAX_TASK_INPUT_TEXT_BYTES + 1), false),
            ("é".repeat(MAX_TASK_INPUT_TEXT_BYTES / 2), true),
            ("é".repeat(MAX_TASK_INPUT_TEXT_BYTES / 2 + 1), false),
            (String::new(), false),
        ] {
            let payload = serde_json::json!({
                "v": 1, "type": "task_submit",
                "connection_id": "connection-1", "controller_lease_id": "lease-1",
                "lease_epoch": "1", "relay_epoch": "relay-1",
                "remote_control_generation": "generation-1",
                "request_id": "request-1", "task_id": "task-1",
                "client_request_id": "client-1", "session_id": "b-session-1",
                "capability_revision": "1", "model_source": "controlled_device",
                "mode": "chat", "input_digest": "a".repeat(64),
                "input": { "text": text, "attachment_ids": [], "skill_ids": [] },
            });
            let protocol = validate_controller_payload_json(
                &serde_json::to_string(&payload).unwrap(),
                SenderRole::ControllerA,
            )
            .is_ok();
            assert_eq!(protocol, expected, "protocol text bytes={}", text.len());
            assert_eq!(validated_local_text(&text).is_ok(), expected);
        }
        assert!(validated_local_text(" \t ").is_err());
    }
}
