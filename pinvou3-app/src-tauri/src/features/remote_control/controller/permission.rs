//! A-only approval authority and the dispatch race boundary (DEV-16).
//!
//! B projects a pending permission to A and can only look at it, stop the task
//! or disconnect. There is no B-side Allow path: [`PermissionAuthority::decide`]
//! takes an explicit [`DecisionAuthority`] and rejects anything that is not the
//! authenticated A controller.
//!
//! RC-C-014 is enforced by a single compare-and-set boundary: an allow only
//! becomes an execution once the caller atomically commits the dispatch. If B
//! stopped the task first the dispatch is rejected; if the dispatch was already
//! committed the result is reported as executed / result-unknown and never as
//! "not executed".

use std::collections::HashMap;
use std::fmt;

use parking_lot::Mutex;
use serde::Serialize;

use super::protocol::LeaseContext;
use super::task::RemoteTaskPermissionScope;

pub(crate) const MAX_PENDING_PERMISSIONS: usize = 8;
pub(crate) const MAX_PERMISSION_ID_CHARS: usize = 128;
pub(crate) const MAX_TASK_ID_CHARS: usize = 128;
pub(crate) const MAX_SCOPE_CHARS: usize = 256;
pub(crate) const MAX_PREVIEW_CHARS: usize = 512;
pub(crate) const MAX_EXPIRY_HORIZON_MS: u64 = 10 * 60 * 1000;

const OPERATION_TYPES: &[&str] = &[
    "file_write",
    "file_delete",
    "shell_command",
    "network_request",
    "tool_call",
    "other",
];
const RISK_LEVELS: &[&str] = &["low", "medium", "high", "critical"];

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct PermissionError {
    pub(crate) code: &'static str,
}

impl PermissionError {
    fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl fmt::Display for PermissionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for PermissionError {}

type Result<T> = std::result::Result<T, PermissionError>;

fn error(code: &'static str) -> PermissionError {
    PermissionError::new(code)
}

/// Only the authenticated A controller may decide. B has view/stop/disconnect
/// rights and is deliberately not representable here.
#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) enum DecisionAuthority {
    ControllerA,
    ControlledB,
    LocalOperator,
}

#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum PermissionDecision {
    Allow,
    Deny,
}

impl PermissionDecision {
    pub(crate) fn parse(value: &str) -> Option<Self> {
        match value {
            "allow" => Some(Self::Allow),
            "deny" => Some(Self::Deny),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct PermissionBinding {
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: String,
    pub(crate) remote_control_generation: String,
    pub(crate) task_id: String,
    pub(crate) permission_request_id: String,
}

impl PermissionBinding {
    pub(crate) fn new(
        lease: &LeaseContext,
        task_id: &str,
        permission_request_id: &str,
    ) -> Result<Self> {
        let binding = Self {
            connection_id: lease.connection_id.clone(),
            controller_lease_id: lease.controller_lease_id.clone(),
            lease_epoch: lease.lease_epoch.clone(),
            remote_control_generation: lease.remote_control_generation.clone(),
            task_id: task_id.to_owned(),
            permission_request_id: permission_request_id.to_owned(),
        };
        binding.validate()?;
        Ok(binding)
    }

    fn validate(&self) -> Result<()> {
        for value in [
            &self.connection_id,
            &self.controller_lease_id,
            &self.lease_epoch,
            &self.remote_control_generation,
            &self.task_id,
            &self.permission_request_id,
        ] {
            if value.is_empty()
                || value.len() > MAX_PERMISSION_ID_CHARS
                || !value
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
            {
                return Err(error("permission_binding_invalid"));
            }
        }
        Ok(())
    }

    fn matches(&self, lease: &LeaseContext, task_id: &str, request_id: &str) -> bool {
        self.connection_id == lease.connection_id
            && self.controller_lease_id == lease.controller_lease_id
            && self.lease_epoch == lease.lease_epoch
            && self.remote_control_generation == lease.remote_control_generation
            && self.task_id == task_id
            && self.permission_request_id == request_id
    }
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct PermissionRequest {
    pub(crate) operation_type: String,
    pub(crate) risk_level: String,
    pub(crate) target_scope: String,
    pub(crate) preview: String,
    pub(crate) expires_at_ms: u64,
}

impl PermissionRequest {
    fn validate(&self, now_b: u64) -> Result<()> {
        if !OPERATION_TYPES.contains(&self.operation_type.as_str()) {
            return Err(error("permission_operation_unsupported"));
        }
        if !RISK_LEVELS.contains(&self.risk_level.as_str()) {
            return Err(error("permission_risk_unsupported"));
        }
        if self.target_scope.trim().is_empty()
            || self.target_scope.chars().count() > MAX_SCOPE_CHARS
        {
            return Err(error("permission_scope_invalid"));
        }
        if self.preview.chars().count() > MAX_PREVIEW_CHARS {
            return Err(error("permission_preview_invalid"));
        }
        if self.expires_at_ms <= now_b
            || self.expires_at_ms.saturating_sub(now_b) > MAX_EXPIRY_HORIZON_MS
        {
            return Err(error("permission_expiry_invalid"));
        }
        Ok(())
    }
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum PermissionState {
    Pending,
    AllowedAwaitingDispatch,
    Denied,
    Expired,
}

/// Where the operation ended up relative to the dispatch boundary.
#[derive(Debug, Clone, Copy, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum DispatchState {
    /// No allow decision, or B stopped before the dispatch was committed.
    NotDispatched,
    /// The operation was atomically dispatched and may have run.
    Dispatched,
}

impl DispatchState {
    fn committed(self) -> bool {
        matches!(self, Self::Dispatched)
    }
}

/// Read-only projection for the B UI: never carries credentials.
#[derive(Debug, Clone, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct PermissionView {
    pub(crate) permission_request_id: String,
    pub(crate) task_id: String,
    pub(crate) operation_type: String,
    pub(crate) risk_level: String,
    pub(crate) target_scope: String,
    pub(crate) preview: String,
    pub(crate) expires_at_ms: u64,
    pub(crate) state: String,
    pub(crate) decision: Option<String>,
    pub(crate) dispatch_state: String,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum DecisionOutcome {
    /// First effective decision for this request.
    Decided {
        decision: PermissionDecision,
        dispatch_state: DispatchState,
    },
    /// Same value retried; idempotent.
    Replayed {
        decision: PermissionDecision,
        dispatch_state: DispatchState,
    },
    /// Arrived after the request reached a terminal state.
    Expired,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum DispatchCommit {
    /// The operation may run now.
    Committed,
    /// A deny, expiry or B-side stop won the race.
    Rejected { code: &'static str },
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum DispatchResult {
    /// Dispatch was never committed, so the operation did not run.
    NotDispatched,
    /// Dispatch was committed; the operation ran or its outcome is unknown.
    ExecutedOrUnknown,
}

struct PendingPermission {
    binding: PermissionBinding,
    request: PermissionRequest,
    state: PermissionState,
    decision: Option<PermissionDecision>,
    dispatch_state: DispatchState,
    resolved_at_ms: Option<u64>,
    resolved_reason: Option<String>,
}

/// Shared permission authority for one B process.
#[derive(Default)]
pub(crate) struct PermissionAuthority {
    pending: Mutex<HashMap<String, PendingPermission>>,
}

impl PermissionAuthority {
    /// Registers a pending permission. B may only have one pending request per
    /// task at a time.
    pub(crate) fn request(
        &self,
        binding: PermissionBinding,
        request: PermissionRequest,
        now_b: u64,
    ) -> Result<PermissionView> {
        binding.validate()?;
        request.validate(now_b)?;
        let request = PermissionRequest {
            preview: sanitize_preview(&request.preview),
            target_scope: sanitize_scope(&request.target_scope),
            ..request
        };
        let mut pending = self.pending.lock();
        if pending.values().any(|existing| {
            existing.binding.task_id == binding.task_id
                && existing.state == PermissionState::Pending
        }) {
            return Err(error("permission_task_busy"));
        }
        if pending.len() >= MAX_PENDING_PERMISSIONS {
            return Err(error("permission_capacity"));
        }
        let id = binding.permission_request_id.clone();
        pending.insert(
            id.clone(),
            PendingPermission {
                binding,
                request,
                state: PermissionState::Pending,
                decision: None,
                dispatch_state: DispatchState::NotDispatched,
                resolved_at_ms: None,
                resolved_reason: None,
            },
        );
        Ok(Self::view_locked(pending.get(&id).expect("just inserted")))
    }

    /// First effective A decision wins. B and local callers are rejected, and a
    /// conflicting second decision fails closed.
    pub(crate) fn decide(
        &self,
        authority: DecisionAuthority,
        lease: &LeaseContext,
        task_id: &str,
        request_id: &str,
        decision: PermissionDecision,
        now_b: u64,
    ) -> Result<DecisionOutcome> {
        if authority != DecisionAuthority::ControllerA {
            return Err(error("permission_authority_invalid"));
        }
        let mut pending = self.pending.lock();
        let entry = pending
            .get_mut(request_id)
            .ok_or_else(|| error("permission_unknown"))?;
        if !entry.binding.matches(lease, task_id, request_id) {
            return Err(error("permission_binding_stale"));
        }
        match entry.state {
            PermissionState::Pending if now_b >= entry.request.expires_at_ms => {
                entry.state = PermissionState::Expired;
                entry.decision = Some(PermissionDecision::Deny);
                entry.resolved_at_ms = Some(now_b);
                entry.resolved_reason = Some("expired".to_owned());
                return Ok(DecisionOutcome::Expired);
            }
            PermissionState::Pending => {
                entry.decision = Some(decision);
                entry.state = match decision {
                    PermissionDecision::Allow => PermissionState::AllowedAwaitingDispatch,
                    PermissionDecision::Deny => PermissionState::Denied,
                };
                entry.resolved_at_ms = Some(now_b);
                entry.resolved_reason = Some(
                    match decision {
                        PermissionDecision::Allow => "allow",
                        PermissionDecision::Deny => "deny",
                    }
                    .to_owned(),
                );
                return Ok(DecisionOutcome::Decided {
                    decision,
                    dispatch_state: entry.dispatch_state,
                });
            }
            PermissionState::Expired => return Ok(DecisionOutcome::Expired),
            PermissionState::AllowedAwaitingDispatch | PermissionState::Denied => {
                let existing = entry.decision.unwrap_or(PermissionDecision::Deny);
                if existing == decision {
                    return Ok(DecisionOutcome::Replayed {
                        decision: existing,
                        dispatch_state: entry.dispatch_state,
                    });
                }
                return Err(error("permission_already_decided"));
            }
        }
    }

    /// Atomic dispatch boundary. `Committed` means the operation may run and
    /// any later B stop must report executed/result-unknown.
    pub(crate) fn commit_dispatch(&self, request_id: &str, now_b: u64) -> Result<DispatchCommit> {
        let mut pending = self.pending.lock();
        let entry = pending
            .get_mut(request_id)
            .ok_or_else(|| error("permission_unknown"))?;
        if now_b >= entry.request.expires_at_ms {
            entry.state = PermissionState::Expired;
            entry.decision = Some(PermissionDecision::Deny);
            entry.resolved_at_ms = Some(now_b);
            entry.resolved_reason = Some("expired".to_owned());
            return Ok(DispatchCommit::Rejected {
                code: "permission_expired",
            });
        }
        match entry.state {
            PermissionState::AllowedAwaitingDispatch => {
                entry.dispatch_state = DispatchState::Dispatched;
                Ok(DispatchCommit::Committed)
            }
            PermissionState::Denied => Ok(DispatchCommit::Rejected {
                code: "permission_denied",
            }),
            PermissionState::Expired => Ok(DispatchCommit::Rejected {
                code: "permission_expired",
            }),
            PermissionState::Pending => Ok(DispatchCommit::Rejected {
                code: "permission_not_decided",
            }),
        }
    }

    /// Reports the real outcome after B stopped the task. Never claims "not
    /// executed" once the dispatch boundary was committed.
    pub(crate) fn dispatch_result(&self, request_id: &str) -> Result<DispatchResult> {
        let pending = self.pending.lock();
        let entry = pending
            .get(request_id)
            .ok_or_else(|| error("permission_unknown"))?;
        Ok(if entry.dispatch_state.committed() {
            DispatchResult::ExecutedOrUnknown
        } else {
            DispatchResult::NotDispatched
        })
    }

    /// B stop / refresh / disconnect / task cancel: deny everything pending for
    /// the task. Returns how many requests changed state.
    pub(crate) fn deny_all_for_task(&self, task_id: &str, reason: &str, now_b: u64) -> usize {
        self.deny_where(|entry| entry.binding.task_id == task_id, reason, now_b)
    }

    pub(crate) fn deny_all_for_connection(
        &self,
        connection_id: &str,
        reason: &str,
        now_b: u64,
    ) -> usize {
        self.deny_where(
            |entry| entry.binding.connection_id == connection_id,
            reason,
            now_b,
        )
    }

    pub(crate) fn sweep_expired(&self, now_b: u64) -> usize {
        let mut pending = self.pending.lock();
        let mut expired = 0;
        for entry in pending.values_mut() {
            if entry.state == PermissionState::Pending && now_b >= entry.request.expires_at_ms {
                entry.state = PermissionState::Expired;
                entry.decision = Some(PermissionDecision::Deny);
                entry.resolved_at_ms = Some(now_b);
                entry.resolved_reason = Some("expired".to_owned());
                expired += 1;
            }
        }
        expired
    }

    pub(crate) fn view(&self, request_id: &str) -> Result<PermissionView> {
        let pending = self.pending.lock();
        pending
            .get(request_id)
            .map(Self::view_locked)
            .ok_or_else(|| error("permission_unknown"))
    }

    pub(crate) fn pending_for_task(&self, task_id: &str) -> Vec<PermissionView> {
        let pending = self.pending.lock();
        let mut views: Vec<PermissionView> = pending
            .values()
            .filter(|entry| entry.binding.task_id == task_id)
            .map(Self::view_locked)
            .collect();
        views.sort_by(|left, right| left.permission_request_id.cmp(&right.permission_request_id));
        views
    }

    pub(crate) fn active_count(&self) -> usize {
        self.pending.lock().len()
    }

    pub(crate) fn clear(&self) {
        self.pending.lock().clear();
    }

    fn deny_where(
        &self,
        predicate: impl Fn(&PendingPermission) -> bool,
        reason: &str,
        now_b: u64,
    ) -> usize {
        let mut pending = self.pending.lock();
        let mut changed = 0;
        for entry in pending.values_mut() {
            if !predicate(entry) {
                continue;
            }
            // Once the dispatch boundary was committed the operation already
            // ran (or its outcome is unknown); a B stop must not rewrite that
            // history into "not executed".
            if entry.dispatch_state == DispatchState::Dispatched {
                continue;
            }
            match entry.state {
                PermissionState::Pending | PermissionState::AllowedAwaitingDispatch => {
                    entry.state = PermissionState::Denied;
                    entry.decision = Some(PermissionDecision::Deny);
                    entry.resolved_at_ms = Some(now_b);
                    entry.resolved_reason = Some(bounded_reason(reason));
                    changed += 1;
                }
                PermissionState::Denied | PermissionState::Expired => {}
            }
        }
        changed
    }

    fn view_locked(entry: &PendingPermission) -> PermissionView {
        PermissionView {
            permission_request_id: entry.binding.permission_request_id.clone(),
            task_id: entry.binding.task_id.clone(),
            operation_type: entry.request.operation_type.clone(),
            risk_level: entry.request.risk_level.clone(),
            target_scope: sanitize_scope(&entry.request.target_scope),
            preview: sanitize_preview(&entry.request.preview),
            expires_at_ms: entry.request.expires_at_ms,
            state: match entry.state {
                PermissionState::Pending => "pending",
                PermissionState::AllowedAwaitingDispatch => "allowed",
                PermissionState::Denied => "denied",
                PermissionState::Expired => "expired",
            }
            .to_owned(),
            decision: entry.decision.map(|decision| {
                match decision {
                    PermissionDecision::Allow => "allow",
                    PermissionDecision::Deny => "deny",
                }
                .to_owned()
            }),
            dispatch_state: match entry.dispatch_state {
                DispatchState::NotDispatched => "not_dispatched",
                DispatchState::Dispatched => "dispatched",
            }
            .to_owned(),
        }
    }
}

impl RemoteTaskPermissionScope for PermissionAuthority {
    fn deny_pending_for_task(&self, task_id: &str) -> usize {
        self.deny_all_for_task(task_id, "task_terminal", 0)
    }
}

/// A preview is unrestricted caller text, so no nonempty source text is safe
/// to project. The fixed sentinel is stable across request storage and UI
/// projection; the empty preview remains empty.
pub(crate) fn sanitize_preview(preview: &str) -> String {
    if preview.is_empty() {
        return String::new();
    }
    if matches!(preview, "[PATH]" | "[REDACTED]") {
        return preview.to_owned();
    }
    "[REDACTED]".to_owned()
}

fn sanitize_scope(scope: &str) -> String {
    let scope = scope.trim();
    // request() stores this value and view_locked() projects it again. Keep
    // both redaction sentinels stable instead of changing their meaning on a
    // second pass.
    if matches!(scope, "[PATH]" | "[REDACTED]") {
        return scope.to_owned();
    }
    // This value comes from a free-form Tauri input, not an authenticated
    // workspace registry. Even a plausible workspace prefix is not proof of
    // authorization, so never echo any caller-controlled scope verbatim.
    if looks_like_path(scope) || scope.contains('/') || scope.contains('\\') || scope.contains(':')
    {
        return "[PATH]".to_owned();
    }
    "[REDACTED]".to_owned()
}

fn looks_like_path(value: &str) -> bool {
    let bytes = value.as_bytes();
    if bytes.len() >= 3
        && bytes[0].is_ascii_alphabetic()
        && bytes[1] == b':'
        && matches!(bytes[2], b'\\' | b'/')
    {
        return true;
    }
    let separators = value.matches(['\\', '/']).count();
    separators >= 2 || (separators == 1 && (value.starts_with('/') || value.contains('\\')))
}

fn bounded_reason(reason: &str) -> String {
    let sanitized: String = reason
        .chars()
        .filter(|ch| ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-'))
        .take(64)
        .collect();
    if sanitized.is_empty() {
        "unspecified".to_owned()
    } else {
        sanitized
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "3".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn request(expires_at_ms: u64) -> PermissionRequest {
        PermissionRequest {
            operation_type: "file_write".into(),
            risk_level: "medium".into(),
            target_scope: "workspace-0001/report.md".into(),
            preview: "write report.md".into(),
            expires_at_ms,
        }
    }

    fn register(
        authority: &PermissionAuthority,
        request_id: &str,
        now_b: u64,
    ) -> PermissionBinding {
        let binding = PermissionBinding::new(&lease(), "task-0001", request_id).unwrap();
        authority
            .request(binding.clone(), request(now_b + 60_000), now_b)
            .unwrap();
        binding
    }

    #[test]
    fn only_a_can_decide_and_b_cannot_allow() {
        let authority = PermissionAuthority::default();
        register(&authority, "permission-0001", 1_000);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControlledB,
                    &lease(),
                    "task-0001",
                    "permission-0001",
                    PermissionDecision::Allow,
                    1_100,
                )
                .unwrap_err()
                .code,
            "permission_authority_invalid"
        );
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::LocalOperator,
                    &lease(),
                    "task-0001",
                    "permission-0001",
                    PermissionDecision::Allow,
                    1_100,
                )
                .unwrap_err()
                .code,
            "permission_authority_invalid"
        );
        assert_eq!(authority.view("permission-0001").unwrap().state, "pending");
    }

    #[test]
    fn first_decision_wins_and_same_value_retries_are_idempotent() {
        let authority = PermissionAuthority::default();
        register(&authority, "permission-0001", 1_000);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    "permission-0001",
                    PermissionDecision::Allow,
                    1_100,
                )
                .unwrap(),
            DecisionOutcome::Decided {
                decision: PermissionDecision::Allow,
                dispatch_state: DispatchState::NotDispatched,
            }
        );
        assert!(matches!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    "permission-0001",
                    PermissionDecision::Allow,
                    1_200,
                )
                .unwrap(),
            DecisionOutcome::Replayed { .. }
        ));
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    "permission-0001",
                    PermissionDecision::Deny,
                    1_300,
                )
                .unwrap_err()
                .code,
            "permission_already_decided"
        );
    }

    #[test]
    fn expired_requests_default_to_deny() {
        let authority = PermissionAuthority::default();
        register(&authority, "permission-0001", 1_000);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    "permission-0001",
                    PermissionDecision::Allow,
                    61_000,
                )
                .unwrap(),
            DecisionOutcome::Expired
        );
        assert_eq!(
            authority
                .commit_dispatch("permission-0001", 62_000)
                .unwrap(),
            DispatchCommit::Rejected {
                code: "permission_expired"
            }
        );
        assert_eq!(authority.view("permission-0001").unwrap().state, "expired");
    }

    #[test]
    fn dispatch_is_rejected_until_a_decided_allow_commits() {
        let authority = PermissionAuthority::default();
        register(&authority, "permission-0001", 1_000);
        assert_eq!(
            authority.commit_dispatch("permission-0001", 1_100).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_not_decided"
            }
        );
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                "permission-0001",
                PermissionDecision::Allow,
                1_200,
            )
            .unwrap();
        assert_eq!(
            authority.commit_dispatch("permission-0001", 1_300).unwrap(),
            DispatchCommit::Committed
        );
        assert_eq!(
            authority.view("permission-0001").unwrap().dispatch_state,
            "dispatched"
        );
    }

    #[test]
    fn deny_never_dispatches() {
        let authority = PermissionAuthority::default();
        register(&authority, "permission-0001", 1_000);
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                "permission-0001",
                PermissionDecision::Deny,
                1_100,
            )
            .unwrap();
        assert_eq!(
            authority.commit_dispatch("permission-0001", 1_200).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_denied"
            }
        );
        assert_eq!(
            authority.dispatch_result("permission-0001").unwrap(),
            DispatchResult::NotDispatched
        );
    }

    #[test]
    fn b_stop_after_dispatch_reports_executed_or_unknown_never_not_executed() {
        // Stop wins the race: the allow decision never reaches the operation.
        let authority = PermissionAuthority::default();
        register(&authority, "permission-before", 1_000);
        assert_eq!(authority.deny_all_for_task("task-0001", "b_stop", 1_100), 1);
        assert_eq!(
            authority
                .commit_dispatch("permission-before", 1_200)
                .unwrap(),
            DispatchCommit::Rejected {
                code: "permission_denied"
            }
        );
        assert_eq!(
            authority.dispatch_result("permission-before").unwrap(),
            DispatchResult::NotDispatched
        );

        // Dispatch wins the race: a later stop must not claim "not executed".
        let authority = PermissionAuthority::default();
        register(&authority, "permission-after", 1_000);
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                "permission-after",
                PermissionDecision::Allow,
                1_100,
            )
            .unwrap();
        assert_eq!(
            authority
                .commit_dispatch("permission-after", 1_200)
                .unwrap(),
            DispatchCommit::Committed
        );
        assert_eq!(authority.deny_all_for_task("task-0001", "b_stop", 1_300), 0);
        assert_eq!(
            authority.dispatch_result("permission-after").unwrap(),
            DispatchResult::ExecutedOrUnknown
        );
    }

    #[test]
    fn cross_task_and_cross_lease_decisions_are_rejected() {
        let authority = PermissionAuthority::default();
        register(&authority, "permission-0001", 1_000);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0002",
                    "permission-0001",
                    PermissionDecision::Allow,
                    1_100,
                )
                .unwrap_err()
                .code,
            "permission_binding_stale"
        );
        let mut other = lease();
        other.controller_lease_id = "lease-0002".into();
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &other,
                    "task-0001",
                    "permission-0001",
                    PermissionDecision::Allow,
                    1_100,
                )
                .unwrap_err()
                .code,
            "permission_binding_stale"
        );
    }

    #[test]
    fn preview_and_scope_are_redacted_before_any_projection() {
        let authority = PermissionAuthority::default();
        let binding = PermissionBinding::new(&lease(), "task-0001", "permission-0001").unwrap();
        let view = authority
            .request(
                binding,
                PermissionRequest {
                    operation_type: "shell_command".into(),
                    risk_level: "high".into(),
                    target_scope: "workspace-0001".into(),
                    preview: "run curl -H 'Authorization: Bearer sk-live-123' \
                              --data @C:\\Users\\me\\secret.txt https://example.com"
                        .into(),
                    expires_at_ms: 61_000,
                },
                1_000,
            )
            .unwrap();
        assert_eq!(view.preview, "[REDACTED]");
        assert_eq!(view.target_scope, "[REDACTED]");
    }

    #[test]
    fn preview_request_view_and_serde_never_echo_free_text() {
        const OPAQUE_KEY: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
        for (raw, expected) in [
            ("", ""),
            ("ordinary request description", "[REDACTED]"),
            ("sk_live_ABC123", "[REDACTED]"),
            (OPAQUE_KEY, "[REDACTED]"),
            ("apiKey=private-value", "[REDACTED]"),
            (r"C:\Users\me\private.txt", "[REDACTED]"),
            ("[PATH]", "[PATH]"),
            ("[REDACTED]", "[REDACTED]"),
        ] {
            assert_eq!(sanitize_preview(raw), expected);
            assert_eq!(sanitize_preview(expected), expected);
            let authority = PermissionAuthority::default();
            let mut input = request(61_000);
            input.preview = raw.to_owned();
            let initial = authority
                .request(
                    PermissionBinding::new(&lease(), "task-0001", "permission-0001").unwrap(),
                    input,
                    1_000,
                )
                .unwrap();
            assert_eq!(initial.preview, expected);
            let read = authority.view("permission-0001").unwrap();
            assert_eq!(read.preview, expected);
            let dto = serde_json::to_value(&read).unwrap();
            assert_eq!(dto["preview"], expected);
            let serialized = dto.to_string();
            if raw != expected {
                assert!(!serialized.contains(raw));
            }
        }
    }

    #[test]
    fn target_scope_projection_never_echoes_unverified_caller_input() {
        const OPAQUE_KEY: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
        for (raw, expected) in [
            ("workspace-0001/docs/report.md", "[PATH]"),
            ("workspace_abc/report.md", "[PATH]"),
            ("workspace-0001", "[REDACTED]"),
            (r"C:\Users\me\private.txt", "[PATH]"),
            ("workspace-0001/../../C:/private.txt", "[PATH]"),
            ("workspace-0001/credentials.json", "[PATH]"),
            ("token=private-value", "[REDACTED]"),
            ("sk-live-private-value", "[REDACTED]"),
            ("sk_live_ABC123", "[REDACTED]"),
            (OPAQUE_KEY, "[REDACTED]"),
            ("arbitrary-opaque-id", "[REDACTED]"),
            ("workspace-0001/sk_live_ABC123", "[PATH]"),
            ("workspace-0001/0123456789abcdef0123456789abcdef", "[PATH]"),
            ("[PATH]", "[PATH]"),
            ("[REDACTED]", "[REDACTED]"),
        ] {
            assert_eq!(sanitize_scope(raw), expected, "first pass scope={raw}");
            assert_eq!(
                sanitize_scope(expected),
                expected,
                "second pass scope={raw}"
            );
            let authority = PermissionAuthority::default();
            let mut input = request(61_000);
            input.target_scope = raw.to_owned();
            let view = authority
                .request(
                    PermissionBinding::new(&lease(), "task-0001", "permission-0001").unwrap(),
                    input,
                    1_000,
                )
                .unwrap();
            assert_eq!(view.target_scope, expected, "scope={raw}");
            assert_eq!(
                authority.view("permission-0001").unwrap().target_scope,
                expected
            );
            let dto = serde_json::to_string(&view).unwrap();
            assert_eq!(
                serde_json::to_value(&view).unwrap()["targetScope"],
                expected,
                "serialized scope={raw}"
            );
            assert!(!dto.contains("private-value"));
            assert!(!dto.contains("private.txt"));
            assert!(!dto.contains("ABC123"));
            assert!(!dto.contains(OPAQUE_KEY));
        }
    }

    #[test]
    fn request_shape_is_bounded_and_unknown_enums_are_rejected() {
        let authority = PermissionAuthority::default();
        let binding = PermissionBinding::new(&lease(), "task-0001", "permission-0001").unwrap();
        let mut bad_operation = request(61_000);
        bad_operation.operation_type = "root_shell".into();
        assert_eq!(
            authority
                .request(binding.clone(), bad_operation, 1_000)
                .unwrap_err()
                .code,
            "permission_operation_unsupported"
        );
        let mut bad_risk = request(61_000);
        bad_risk.risk_level = "unknown".into();
        assert_eq!(
            authority
                .request(binding.clone(), bad_risk, 1_000)
                .unwrap_err()
                .code,
            "permission_risk_unsupported"
        );
        let mut expired = request(1_000);
        expired.expires_at_ms = 1_000;
        assert_eq!(
            authority
                .request(binding.clone(), expired, 1_000)
                .unwrap_err()
                .code,
            "permission_expiry_invalid"
        );
        let mut far = request(61_000);
        far.expires_at_ms = 1_000 + MAX_EXPIRY_HORIZON_MS + 1;
        assert_eq!(
            authority.request(binding, far, 1_000).unwrap_err().code,
            "permission_expiry_invalid"
        );
    }

    #[test]
    fn only_one_pending_request_per_task_is_allowed() {
        let authority = PermissionAuthority::default();
        register(&authority, "permission-0001", 1_000);
        let second = PermissionBinding::new(&lease(), "task-0001", "permission-0002").unwrap();
        assert_eq!(
            authority
                .request(second, request(61_000), 1_000)
                .unwrap_err()
                .code,
            "permission_task_busy"
        );
    }

    #[test]
    fn concurrent_same_value_decisions_commit_one_terminal_state() {
        let authority = Arc::new(PermissionAuthority::default());
        register(&authority, "permission-0001", 1_000);
        let mut handles = Vec::new();
        for _ in 0..8 {
            let authority = Arc::clone(&authority);
            handles.push(std::thread::spawn(move || {
                authority.decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    "permission-0001",
                    PermissionDecision::Allow,
                    1_100,
                )
            }));
        }
        let mut decided = 0;
        let mut replayed = 0;
        for handle in handles {
            match handle.join().unwrap().unwrap() {
                DecisionOutcome::Decided { .. } => decided += 1,
                DecisionOutcome::Replayed { .. } => replayed += 1,
                DecisionOutcome::Expired => panic!("unexpected expiry"),
            }
        }
        assert_eq!(decided, 1);
        assert_eq!(replayed, 7);
    }

    #[test]
    fn terminal_cleanup_hook_denies_pending_requests() {
        let authority = PermissionAuthority::default();
        register(&authority, "permission-0001", 1_000);
        let scope: &dyn RemoteTaskPermissionScope = &authority;
        assert_eq!(scope.deny_pending_for_task("task-0001"), 1);
        assert_eq!(scope.deny_pending_for_task("task-0001"), 0);
        assert_eq!(authority.view("permission-0001").unwrap().state, "denied");
        assert_eq!(
            authority.commit_dispatch("permission-0001", 1_200).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_denied"
            }
        );
    }
}
