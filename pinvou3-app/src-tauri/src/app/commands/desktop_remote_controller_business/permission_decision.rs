//! Shared production handler. Tauri only supplies its real managed owners;
//! the ACP response is the sole asynchronous dependency boundary.
use super::{
    DecisionAuthority, DecisionOutcome, LeaseContext, PermissionAuthority, PermissionDecision,
    PermissionDecisionOwner, RemotePermissionOwner, RemoteTaskService, TaskLeaseBinding, Value,
    json,
};
use crate::features::remote_control::controller::task::RemoteTaskState;

pub(super) struct PermissionDecisionHandler<'a> {
    pub(super) controlled: &'a super::ControlledDeviceManager,
    pub(super) tasks: &'a RemoteTaskService,
    pub(super) authority: &'a PermissionAuthority,
}

#[cfg(test)]
#[path = "permission_decision_tests.rs"]
mod tests;

enum Admission {
    Receipt(String),
    Dispatch {
        owner: RemotePermissionOwner,
        request_id: String,
        effective: PermissionDecision,
        requested: PermissionDecision,
    },
}

impl PermissionDecisionHandler<'_> {
    pub(super) async fn handle<F, Fut>(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        body: &Value,
        respond: F,
    ) -> Result<Value, String>
    where
        F: FnOnce(RemotePermissionOwner, String, PermissionDecision) -> Fut + Send,
        Fut: std::future::Future<Output = Result<(), String>> + Send,
    {
        let admission = self
            .controlled
            .with_current_business_lease(lease, || {
                let field = |key: &str| {
                    body.get(key)
                        .and_then(Value::as_str)
                        .ok_or_else(|| "remote_action_invalid".to_owned())
                };
                let task_id = field("task_id")?;
                let request_id = field("permission_request_id")?;
                let decision = PermissionDecision::parse(field("decision")?)
                    .ok_or_else(|| "permission_decision_invalid".to_owned())?;
                let session_id = self
                    .tasks
                    .session_for(session_ref, &lease.remote_control_generation)
                    .ok_or_else(|| "task_session_binding_mismatch".to_owned())?;
                let record = self
                    .tasks
                    .ledger()
                    .record(task_id)
                    .map_err(|error| error.code.to_owned())?;
                if record.session_id != session_id
                    || record.lease != TaskLeaseBinding::from_context(lease)
                {
                    return Err("task_session_binding_mismatch".to_owned());
                }
                let receipt_owner = PermissionDecisionOwner {
                    session_id: session_id.clone(),
                    task_sequence: record.sequence,
                };
                if let Some(view) = self
                    .authority
                    .decision_receipt(lease, task_id, request_id, decision, &receipt_owner)
                    .map_err(|error| error.code.to_owned())?
                {
                    // Read an already committed decision. No dispatch or mutable
                    // authority call is allowed on this path, including terminal tasks.
                    return Ok(Admission::Receipt(view.state));
                }
                if !matches!(
                    record.state,
                    RemoteTaskState::Running | RemoteTaskState::WaitingPermission
                ) {
                    return Err("task_session_binding_mismatch".to_owned());
                }
                let outcome = self
                    .authority
                    .decide_for_owner(
                        DecisionAuthority::ControllerA,
                        lease,
                        task_id,
                        request_id,
                        decision,
                        &receipt_owner,
                    )
                    .map_err(|error| error.code.to_owned())?;
                // A decision reserves Allow; the ACP adapter commits only while
                // synchronously handing the response to its exact live owner.
                Ok(Admission::Dispatch {
                    owner: RemotePermissionOwner {
                        task_sequence: record.sequence,
                        session_id,
                        task_id: task_id.to_owned(),
                        lease: lease.clone(),
                    },
                    request_id: request_id.to_owned(),
                    effective: if matches!(outcome, DecisionOutcome::Expired) {
                        PermissionDecision::Deny
                    } else {
                        decision
                    },
                    requested: decision,
                })
            })
            .map_err(|error| error.code.to_owned())??;
        let (owner, request_id, effective, requested) = match admission {
            Admission::Receipt(state) => return Ok(json!({"state":state})),
            Admission::Dispatch {
                owner,
                request_id,
                effective,
                requested,
            } => (owner, request_id, effective, requested),
        };
        // Preserve the first response error. Only an actually committed ACP send
        // grants a read-only receipt; a failed pre-send attempt is not sent.
        respond(owner.clone(), request_id.clone(), effective).await?;
        self.controlled
            .with_current_business_lease(lease, || {
                let record = self
                    .tasks
                    .ledger()
                    .record(&owner.task_id)
                    .map_err(|error| error.code.to_owned())?;
                if record.sequence != owner.task_sequence
                    || record.session_id != owner.session_id
                    || record.lease != TaskLeaseBinding::from_context(lease)
                {
                    return Err("task_session_binding_mismatch".to_owned());
                }
                let receipt_owner = PermissionDecisionOwner {
                    session_id: owner.session_id.clone(),
                    task_sequence: owner.task_sequence,
                };
                let view = self
                    .authority
                    .decision_receipt(
                        lease,
                        &owner.task_id,
                        &request_id,
                        requested,
                        &receipt_owner,
                    )
                    .map_err(|error| error.code.to_owned())?
                    .ok_or_else(|| "permission_dispatch_failed".to_owned())?;
                Ok(json!({"state":view.state}))
            })
            .map_err(|error| error.code.to_owned())?
    }
}
