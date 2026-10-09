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
//!
//! B mints opaque IDs using a process nonce and a checked, process-wide sequence.
//! Callers cannot choose/re-register an ID, including after GC or `clear`.
//! Only active approvals consume the eight slots. Terminal results are retained
//! for at least ten minutes and until their original expiry, within a 256-entry
//! bound; a full retention window fails closed temporarily, never evicts early.

use std::collections::HashMap;
use std::fmt;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, OnceLock};
use std::time::Instant;

use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use super::protocol::LeaseContext;
use super::task::RemoteTaskPermissionScope;

pub(crate) const MAX_PENDING_PERMISSIONS: usize = 8;
pub(crate) const MAX_PERMISSION_ID_CHARS: usize = 128;
pub(crate) const MAX_TASK_ID_CHARS: usize = 128;
pub(crate) const MAX_SCOPE_CHARS: usize = 256;
pub(crate) const MAX_PREVIEW_CHARS: usize = 512;
pub(crate) const MAX_EXPIRY_HORIZON_MS: u64 = 10 * 60 * 1000;
pub(crate) const TERMINAL_RETENTION_MS: u64 = MAX_EXPIRY_HORIZON_MS;
pub(crate) const MAX_RETAINED_PERMISSIONS: usize = 256;

pub(crate) fn permission_monotonic_millis() -> u64 {
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    u64::try_from(ORIGIN.get_or_init(Instant::now).elapsed().as_millis()).unwrap_or(u64::MAX)
}

fn next_sequence(sequence: &AtomicU64) -> Result<u64> {
    // Keep the MSRV without the deprecated fetch_update/newer try_update API.
    let mut previous = sequence.load(Ordering::Relaxed);
    loop {
        let next = previous
            .checked_add(1)
            .ok_or_else(|| error("permission_id_exhausted"))?;
        match sequence.compare_exchange_weak(previous, next, Ordering::Relaxed, Ordering::Relaxed) {
            Ok(_) => return Ok(next),
            Err(current) => previous = current,
        }
    }
}

fn mint_permission_id() -> Result<String> {
    static NONCE: OnceLock<Uuid> = OnceLock::new();
    static SEQUENCE: AtomicU64 = AtomicU64::new(0);
    let sequence = next_sequence(&SEQUENCE)?;
    Ok(format!(
        "permission-{}-{sequence:020}",
        NONCE.get_or_init(Uuid::new_v4).simple()
    ))
}

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
struct PermissionBinding {
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: String,
    relay_epoch: String,
    pub(crate) remote_control_generation: String,
    pub(crate) task_id: String,
    pub(crate) permission_request_id: String,
}

impl PermissionBinding {
    fn new(lease: &LeaseContext, task_id: &str, permission_request_id: &str) -> Result<Self> {
        let binding = Self {
            connection_id: lease.connection_id.clone(),
            controller_lease_id: lease.controller_lease_id.clone(),
            lease_epoch: lease.lease_epoch.clone(),
            relay_epoch: lease.relay_epoch.clone(),
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
            &self.relay_epoch,
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
            && self.relay_epoch == lease.relay_epoch
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

/// Local B-side registration input, shared with the Tauri adapter. Approval IDs
/// are output-only: deny_unknown_fields rejects legacy caller-chosen IDs.
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct PermissionRequestInput {
    pub(crate) task_id: String,
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

impl PendingPermission {
    fn active(&self) -> bool {
        !self.dispatch_state.committed()
            && matches!(
                self.state,
                PermissionState::Pending | PermissionState::AllowedAwaitingDispatch
            )
    }

    fn expire(&mut self, now_b: u64) -> bool {
        if !self.active() || now_b < self.request.expires_at_ms {
            return false;
        }
        self.state = PermissionState::Expired;
        self.decision = Some(PermissionDecision::Deny);
        self.resolved_at_ms = Some(self.request.expires_at_ms);
        self.resolved_reason = Some("expired".into());
        true
    }
}

/// Shared permission authority for one B process.
pub(crate) struct PermissionAuthority {
    pending: Mutex<HashMap<String, PendingPermission>>,
    clock: Arc<dyn Fn() -> u64 + Send + Sync>,
}

impl Default for PermissionAuthority {
    fn default() -> Self {
        Self {
            pending: Mutex::new(HashMap::new()),
            clock: Arc::new(permission_monotonic_millis),
        }
    }
}

impl PermissionAuthority {
    pub(crate) fn now_ms(&self) -> u64 {
        (self.clock)()
    }

    #[cfg(test)]
    fn with_clock(clock: Arc<dyn Fn() -> u64 + Send + Sync>) -> Self {
        Self {
            clock,
            ..Self::default()
        }
    }
    /// Registers a pending permission. B may only have one pending request per
    /// task at a time.
    pub(crate) fn request(
        &self,
        lease: &LeaseContext,
        task_id: &str,
        request: PermissionRequest,
        now_b: u64,
    ) -> Result<PermissionView> {
        request.validate(now_b)?;
        let binding = PermissionBinding::new(lease, task_id, &mint_permission_id()?)?;
        let request = PermissionRequest {
            preview: sanitize_preview(&request.preview),
            target_scope: sanitize_scope(&request.target_scope),
            ..request
        };
        let mut pending = self.pending.lock();
        Self::compact(&mut pending, now_b);
        if pending
            .values()
            .any(|existing| existing.binding.task_id == binding.task_id && existing.active())
        {
            return Err(error("permission_task_busy"));
        }
        if pending.values().filter(|entry| entry.active()).count() >= MAX_PENDING_PERMISSIONS {
            return Err(error("permission_capacity"));
        }
        if pending.len() >= MAX_RETAINED_PERMISSIONS {
            return Err(error("permission_retention_capacity"));
        }
        let id = binding.permission_request_id.clone();
        let inserted = pending.entry(id).insert_entry(PendingPermission {
            binding,
            request,
            state: PermissionState::Pending,
            decision: None,
            dispatch_state: DispatchState::NotDispatched,
            resolved_at_ms: None,
            resolved_reason: None,
        });
        Ok(Self::view_locked(inserted.get()))
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
        entry.expire(now_b);
        match entry.state {
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
        if entry.dispatch_state.committed() {
            return Ok(DispatchCommit::Rejected {
                code: "permission_already_dispatched",
            });
        }
        entry.expire(now_b);
        match entry.state {
            PermissionState::AllowedAwaitingDispatch => {
                entry.dispatch_state = DispatchState::Dispatched;
                entry.resolved_at_ms = Some(now_b);
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
        Self::compact(&mut pending, now_b)
    }

    fn compact(pending: &mut HashMap<String, PendingPermission>, now_b: u64) -> usize {
        let expired = pending
            .values_mut()
            .map(|entry| usize::from(entry.expire(now_b)))
            .sum();
        pending.retain(|_, entry| {
            entry.active()
                || now_b
                    <= entry.request.expires_at_ms.max(
                        entry
                            .resolved_at_ms
                            .unwrap_or(now_b)
                            .saturating_add(TERMINAL_RETENTION_MS),
                    )
        });
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
        let mut pending = self.pending.lock();
        Self::compact(&mut pending, self.now_ms());
        pending.values().filter(|entry| entry.active()).count()
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
            target_scope: entry.request.target_scope.clone(),
            preview: entry.request.preview.clone(),
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
        self.deny_all_for_task(task_id, "task_terminal", self.now_ms())
    }
}

/// Strips credentials, absolute host paths and header values from anything the
/// B UI or the wire preview may show.
pub(crate) fn sanitize_preview(preview: &str) -> String {
    super::redaction::sanitize_text(preview, MAX_PREVIEW_CHARS)
}

fn sanitize_scope(scope: &str) -> String {
    scope
        .chars()
        .filter(|ch| !ch.is_control())
        .take(MAX_SCOPE_CHARS)
        .collect()
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

    #[test]
    fn registration_input_rejects_caller_chosen_ids() {
        let input = serde_json::json!({
            "taskId": "task-0001", "operationType": "file_write", "riskLevel": "medium",
            "targetScope": "workspace-0001", "preview": "write", "expiresAtMs": 60_000
        });
        assert!(serde_json::from_value::<PermissionRequestInput>(input.clone()).is_ok());
        let mut injected = input;
        injected["permissionRequestId"] = serde_json::json!("old-request-id");
        assert!(serde_json::from_value::<PermissionRequestInput>(injected).is_err());
    }

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

    fn register(authority: &PermissionAuthority, now_b: u64) -> String {
        authority
            .request(&lease(), "task-0001", request(now_b + 60_000), now_b)
            .unwrap()
            .permission_request_id
    }

    fn clocked(now: u64) -> (PermissionAuthority, Arc<AtomicU64>) {
        let clock = Arc::new(AtomicU64::new(now));
        let observed = clock.clone();
        (
            PermissionAuthority::with_clock(Arc::new(move || observed.load(Ordering::SeqCst))),
            clock,
        )
    }

    #[test]
    fn returned_permission_view_matches_the_committed_pending_entry() {
        let (authority, _) = clocked(1_000);
        let view = authority
            .request(&lease(), "task-0001", request(61_000), 1_000)
            .unwrap();
        assert_eq!(view.state, "pending");
        assert_eq!(view.decision, None);
        assert_eq!(view.dispatch_state, "not_dispatched");
        assert_eq!(view.task_id, "task-0001");
        assert_eq!(view, authority.view(&view.permission_request_id).unwrap());
        assert_eq!(authority.pending_for_task("task-0001"), vec![view]);
    }

    #[test]
    fn sequential_approvals_recover_slots_and_gc_stays_bounded_without_reusing_ids() {
        let (authority, clock) = clocked(1_000);
        let mut previous_ids = std::collections::HashSet::new();
        let mut first = None;
        for index in 0..1_024 {
            let now = 1_000 + index * 10_000;
            clock.store(now, Ordering::SeqCst);
            let id = register(&authority, now);
            if first.is_none() {
                first = Some(id.clone());
            }
            assert!(previous_ids.insert(id.clone()));
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &id,
                    PermissionDecision::Allow,
                    now + 1,
                )
                .unwrap();
            assert_eq!(
                authority.active_count(),
                1,
                "allow awaiting dispatch still occupies a slot"
            );
            assert_eq!(
                authority.commit_dispatch(&id, now + 2).unwrap(),
                DispatchCommit::Committed
            );
            assert_eq!(authority.active_count(), 0);
            assert!(
                authority.pending.lock().len() < 64,
                "bounded ten-minute history"
            );
        }
        let now = clock.load(Ordering::SeqCst);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &first.unwrap(),
                    PermissionDecision::Allow,
                    now
                )
                .unwrap_err()
                .code,
            "permission_unknown"
        );
    }

    #[test]
    fn eight_active_requests_include_uncommitted_allow_and_expire_without_a_timer() {
        let (authority, clock) = clocked(1_000);
        let mut ids = Vec::new();
        for index in 0..MAX_PENDING_PERMISSIONS {
            let task = format!("task-{index}");
            ids.push(
                authority
                    .request(&lease(), &task, request(61_000), 1_000)
                    .unwrap()
                    .permission_request_id,
            );
        }
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0",
                &ids[0],
                PermissionDecision::Allow,
                1_100,
            )
            .unwrap();
        assert_eq!(authority.active_count(), 8);
        assert_eq!(
            authority
                .request(&lease(), "task-0", request(61_000), 1_100)
                .unwrap_err()
                .code,
            "permission_task_busy"
        );
        assert_eq!(
            authority
                .request(&lease(), "task-extra", request(61_000), 1_100)
                .unwrap_err()
                .code,
            "permission_capacity"
        );
        clock.store(61_000, Ordering::SeqCst);
        let replacement = authority
            .request(&lease(), "task-0", request(121_000), 61_000)
            .unwrap();
        assert_eq!(authority.active_count(), 1);
        assert_ne!(replacement.permission_request_id, ids[0]);
        assert_eq!(
            authority.commit_dispatch(&ids[0], 61_000).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_expired"
            }
        );
    }

    #[test]
    fn terminal_retention_cap_fails_closed_then_recovers_after_the_full_window() {
        let (authority, clock) = clocked(1_000);
        let mut first = None;
        for _ in 0..MAX_RETAINED_PERMISSIONS {
            let id = register(&authority, 1_000);
            if first.is_none() {
                first = Some(id.clone());
            }
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &id,
                    PermissionDecision::Deny,
                    1_000,
                )
                .unwrap();
        }
        assert_eq!(authority.active_count(), 0);
        assert_eq!(
            authority
                .request(&lease(), "task-0001", request(61_000), 1_000)
                .unwrap_err()
                .code,
            "permission_retention_capacity"
        );
        let boundary = 1_000 + TERMINAL_RETENTION_MS;
        clock.store(boundary, Ordering::SeqCst);
        assert_eq!(
            authority
                .request(&lease(), "task-0001", request(boundary + 60_000), boundary)
                .unwrap_err()
                .code,
            "permission_retention_capacity"
        );
        let old = first.unwrap();
        assert!(matches!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &old,
                    PermissionDecision::Deny,
                    boundary
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
                    &old,
                    PermissionDecision::Allow,
                    boundary
                )
                .unwrap_err()
                .code,
            "permission_already_decided"
        );
        clock.store(boundary + 1, Ordering::SeqCst);
        let new = authority
            .request(
                &lease(),
                "task-0001",
                request(boundary + 60_000),
                boundary + 1,
            )
            .unwrap();
        assert_ne!(old, new.permission_request_id);
        assert_eq!(authority.pending.lock().len(), 1);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &old,
                    PermissionDecision::Allow,
                    boundary + 2
                )
                .unwrap_err()
                .code,
            "permission_unknown"
        );
        assert_eq!(
            authority.view(&new.permission_request_id).unwrap().state,
            "pending"
        );
        assert_eq!(
            authority
                .commit_dispatch(&new.permission_request_id, boundary + 2)
                .unwrap(),
            DispatchCommit::Rejected {
                code: "permission_not_decided"
            }
        );
    }

    #[test]
    fn clear_and_new_authorities_do_not_reset_the_process_id_floor() {
        let (authority, _) = clocked(1_000);
        let first = register(&authority, 1_000);
        authority.clear();
        let second = register(&authority, 1_000);
        let third = register(&PermissionAuthority::default(), 1_000);
        let sequence = |id: &str| id.rsplit('-').next().unwrap().parse::<u64>().unwrap();
        assert!(sequence(&first) < sequence(&second));
        assert!(sequence(&second) < sequence(&third));
        assert_eq!(first.len(), 64);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &first,
                    PermissionDecision::Allow,
                    1_001
                )
                .unwrap_err()
                .code,
            "permission_unknown"
        );
        let exhausted = AtomicU64::new(u64::MAX);
        assert_eq!(
            next_sequence(&exhausted).unwrap_err().code,
            "permission_id_exhausted"
        );
        assert_eq!(exhausted.load(Ordering::Relaxed), u64::MAX);
    }

    #[test]
    fn concurrent_sequence_claims_are_unique_and_exhaustion_is_permanent() {
        let sequence = AtomicU64::new(u64::MAX - 32);
        let mut claims = std::thread::scope(|scope| {
            let handles = (0..64)
                .map(|_| scope.spawn(|| next_sequence(&sequence)))
                .collect::<Vec<_>>();
            handles
                .into_iter()
                .filter_map(|handle| handle.join().unwrap().ok())
                .collect::<Vec<_>>()
        });
        claims.sort_unstable();
        assert_eq!(claims, ((u64::MAX - 31)..=u64::MAX).collect::<Vec<_>>());
        assert_eq!(sequence.load(Ordering::Relaxed), u64::MAX);
        assert_eq!(
            next_sequence(&sequence).unwrap_err().code,
            "permission_id_exhausted"
        );
    }

    #[test]
    fn dispatched_history_is_never_rewritten_or_dispatched_twice_even_after_expiry() {
        let (authority, clock) = clocked(1_000);
        let id = register(&authority, 1_000);
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                &id,
                PermissionDecision::Allow,
                1_100,
            )
            .unwrap();
        assert_eq!(
            authority.commit_dispatch(&id, 1_200).unwrap(),
            DispatchCommit::Committed
        );
        clock.store(62_000, Ordering::SeqCst);
        authority.sweep_expired(62_000);
        assert_eq!(authority.deny_all_for_task("task-0001", "stop", 62_000), 0);
        assert_eq!(
            authority.commit_dispatch(&id, 62_000).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_already_dispatched"
            }
        );
        assert_eq!(
            authority.dispatch_result(&id).unwrap(),
            DispatchResult::ExecutedOrUnknown
        );
        assert!(matches!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &id,
                    PermissionDecision::Allow,
                    62_000
                )
                .unwrap(),
            DecisionOutcome::Replayed {
                dispatch_state: DispatchState::Dispatched,
                ..
            }
        ));
        authority.sweep_expired(1_200 + TERMINAL_RETENTION_MS + 1);
        assert_eq!(
            authority.dispatch_result(&id).unwrap_err().code,
            "permission_unknown",
            "GC cannot report not dispatched"
        );
    }

    #[test]
    fn concurrent_dispatch_commits_exactly_once() {
        let authority = Arc::new(PermissionAuthority::default());
        let id = register(&authority, 1_000);
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                &id,
                PermissionDecision::Allow,
                1_100,
            )
            .unwrap();
        let threads: Vec<_> = (0..8)
            .map(|_| {
                let authority = authority.clone();
                let id = id.clone();
                std::thread::spawn(move || authority.commit_dispatch(&id, 1_200).unwrap())
            })
            .collect();
        let mut committed = 0;
        for thread in threads {
            if thread.join().unwrap() == DispatchCommit::Committed {
                committed += 1;
            }
        }
        assert_eq!(committed, 1);
    }

    #[test]
    fn concurrent_registration_cannot_exceed_capacity_or_mint_duplicate_ids() {
        let (authority, _) = clocked(1_000);
        let authority = Arc::new(authority);
        let threads: Vec<_> = (0..32)
            .map(|index| {
                let authority = authority.clone();
                std::thread::spawn(move || {
                    authority.request(&lease(), &format!("task-{index}"), request(61_000), 1_000)
                })
            })
            .collect();
        let mut ids = std::collections::HashSet::new();
        let mut refused = 0;
        for thread in threads {
            match thread.join().unwrap() {
                Ok(view) => assert!(ids.insert(view.permission_request_id)),
                Err(error) => {
                    assert_eq!(error.code, "permission_capacity");
                    refused += 1;
                }
            }
        }
        assert_eq!(ids.len(), 8);
        assert_eq!(refused, 24);
        assert_eq!(authority.active_count(), 8);
        assert_eq!(authority.pending.lock().len(), 8);
    }

    #[test]
    fn allow_cannot_dispatch_if_the_shared_clock_crosses_expiry() {
        let (authority, clock) = clocked(60_999);
        let id = authority
            .request(&lease(), "task-0001", request(61_000), authority.now_ms())
            .unwrap()
            .permission_request_id;
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                &id,
                PermissionDecision::Allow,
                authority.now_ms(),
            )
            .unwrap();
        clock.store(61_000, Ordering::SeqCst);
        assert_eq!(
            authority.commit_dispatch(&id, authority.now_ms()).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_expired"
            }
        );
        assert_eq!(
            authority.dispatch_result(&id).unwrap(),
            DispatchResult::NotDispatched
        );
        assert_eq!(authority.view(&id).unwrap().state, "expired");
    }

    #[test]
    fn task_cleanup_uses_the_injected_clock_and_old_relay_binding_is_rejected() {
        let (authority, clock) = clocked(1_000_000);
        let id = register(&authority, 1_000_000);
        let mut wrong_relay = lease();
        wrong_relay.relay_epoch = "relay-epoch-old".into();
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &wrong_relay,
                    "task-0001",
                    &id,
                    PermissionDecision::Allow,
                    1_000_001
                )
                .unwrap_err()
                .code,
            "permission_binding_stale"
        );
        clock.store(1_000_100, Ordering::SeqCst);
        RemoteTaskPermissionScope::deny_pending_for_task(&authority, "task-0001");
        assert_eq!(
            authority.pending.lock()[&id].resolved_at_ms,
            Some(1_000_100)
        );
        authority.sweep_expired(1_000_100 + TERMINAL_RETENTION_MS);
        assert_eq!(authority.view(&id).unwrap().state, "denied");
        authority.sweep_expired(1_000_100 + TERMINAL_RETENTION_MS + 1);
        assert_eq!(authority.view(&id).unwrap_err().code, "permission_unknown");
    }

    #[test]
    fn only_a_can_decide_and_b_cannot_allow() {
        let authority = PermissionAuthority::default();
        let request_id = register(&authority, 1_000);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControlledB,
                    &lease(),
                    "task-0001",
                    &request_id,
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
                    &request_id,
                    PermissionDecision::Allow,
                    1_100,
                )
                .unwrap_err()
                .code,
            "permission_authority_invalid"
        );
        assert_eq!(authority.view(&request_id).unwrap().state, "pending");
    }

    #[test]
    fn first_decision_wins_and_same_value_retries_are_idempotent() {
        let authority = PermissionAuthority::default();
        let request_id = register(&authority, 1_000);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &request_id,
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
                    &request_id,
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
                    &request_id,
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
        let request_id = register(&authority, 1_000);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &request_id,
                    PermissionDecision::Allow,
                    61_000,
                )
                .unwrap(),
            DecisionOutcome::Expired
        );
        assert_eq!(
            authority.commit_dispatch(&request_id, 62_000).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_expired"
            }
        );
        assert_eq!(authority.view(&request_id).unwrap().state, "expired");
    }

    #[test]
    fn dispatch_is_rejected_until_a_decided_allow_commits() {
        let authority = PermissionAuthority::default();
        let request_id = register(&authority, 1_000);
        assert_eq!(
            authority.commit_dispatch(&request_id, 1_100).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_not_decided"
            }
        );
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                &request_id,
                PermissionDecision::Allow,
                1_200,
            )
            .unwrap();
        assert_eq!(
            authority.commit_dispatch(&request_id, 1_300).unwrap(),
            DispatchCommit::Committed
        );
        assert_eq!(
            authority.view(&request_id).unwrap().dispatch_state,
            "dispatched"
        );
    }

    #[test]
    fn deny_never_dispatches() {
        let authority = PermissionAuthority::default();
        let request_id = register(&authority, 1_000);
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                &request_id,
                PermissionDecision::Deny,
                1_100,
            )
            .unwrap();
        assert_eq!(
            authority.commit_dispatch(&request_id, 1_200).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_denied"
            }
        );
        assert_eq!(
            authority.dispatch_result(&request_id).unwrap(),
            DispatchResult::NotDispatched
        );
    }

    #[test]
    fn b_stop_after_dispatch_reports_executed_or_unknown_never_not_executed() {
        // Stop wins the race: the allow decision never reaches the operation.
        let authority = PermissionAuthority::default();
        let request_id = register(&authority, 1_000);
        assert_eq!(authority.deny_all_for_task("task-0001", "b_stop", 1_100), 1);
        assert_eq!(
            authority.commit_dispatch(&request_id, 1_200).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_denied"
            }
        );
        assert_eq!(
            authority.dispatch_result(&request_id).unwrap(),
            DispatchResult::NotDispatched
        );

        // Dispatch wins the race: a later stop must not claim "not executed".
        let authority = PermissionAuthority::default();
        let request_id = register(&authority, 1_000);
        authority
            .decide(
                DecisionAuthority::ControllerA,
                &lease(),
                "task-0001",
                &request_id,
                PermissionDecision::Allow,
                1_100,
            )
            .unwrap();
        assert_eq!(
            authority.commit_dispatch(&request_id, 1_200).unwrap(),
            DispatchCommit::Committed
        );
        assert_eq!(authority.deny_all_for_task("task-0001", "b_stop", 1_300), 0);
        assert_eq!(
            authority.dispatch_result(&request_id).unwrap(),
            DispatchResult::ExecutedOrUnknown
        );
    }

    #[test]
    fn cross_task_and_cross_lease_decisions_are_rejected() {
        let authority = PermissionAuthority::default();
        let request_id = register(&authority, 1_000);
        assert_eq!(
            authority
                .decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0002",
                    &request_id,
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
                    &request_id,
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
        let view = authority
            .request(
                &lease(),
                "task-0001",
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
        assert!(!view.preview.contains("sk-live-123"));
        assert!(!view.preview.to_ascii_lowercase().contains("bearer"));
        assert!(!view.preview.contains("secret.txt"));
        assert!(view.preview.contains("[REDACTED]"));
        assert!(view.preview.contains("[PATH]"));
    }

    #[test]
    fn request_shape_is_bounded_and_unknown_enums_are_rejected() {
        let authority = PermissionAuthority::default();
        let mut bad_operation = request(61_000);
        bad_operation.operation_type = "root_shell".into();
        assert_eq!(
            authority
                .request(&lease(), "task-0001", bad_operation, 1_000)
                .unwrap_err()
                .code,
            "permission_operation_unsupported"
        );
        let mut bad_risk = request(61_000);
        bad_risk.risk_level = "unknown".into();
        assert_eq!(
            authority
                .request(&lease(), "task-0001", bad_risk, 1_000)
                .unwrap_err()
                .code,
            "permission_risk_unsupported"
        );
        let mut expired = request(1_000);
        expired.expires_at_ms = 1_000;
        assert_eq!(
            authority
                .request(&lease(), "task-0001", expired, 1_000)
                .unwrap_err()
                .code,
            "permission_expiry_invalid"
        );
        let mut far = request(61_000);
        far.expires_at_ms = 1_000 + MAX_EXPIRY_HORIZON_MS + 1;
        assert_eq!(
            authority
                .request(&lease(), "task-0001", far, 1_000)
                .unwrap_err()
                .code,
            "permission_expiry_invalid"
        );
    }

    #[test]
    fn only_one_pending_request_per_task_is_allowed() {
        let authority = PermissionAuthority::default();
        let _request_id = register(&authority, 1_000);
        assert_eq!(
            authority
                .request(&lease(), "task-0001", request(61_000), 1_000)
                .unwrap_err()
                .code,
            "permission_task_busy"
        );
    }

    #[test]
    fn concurrent_same_value_decisions_commit_one_terminal_state() {
        let authority = Arc::new(PermissionAuthority::default());
        let request_id = register(&authority, 1_000);
        let mut handles = Vec::new();
        for _ in 0..8 {
            let authority = Arc::clone(&authority);
            let request_id = request_id.clone();
            handles.push(std::thread::spawn(move || {
                authority.decide(
                    DecisionAuthority::ControllerA,
                    &lease(),
                    "task-0001",
                    &request_id,
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
        let authority = PermissionAuthority::with_clock(Arc::new(|| 1_100));
        let request_id = register(&authority, 1_000);
        let scope: &dyn RemoteTaskPermissionScope = &authority;
        assert_eq!(scope.deny_pending_for_task("task-0001"), 1);
        assert_eq!(scope.deny_pending_for_task("task-0001"), 0);
        assert_eq!(authority.view(&request_id).unwrap().state, "denied");
        assert_eq!(
            authority.commit_dispatch(&request_id, 1_200).unwrap(),
            DispatchCommit::Rejected {
                code: "permission_denied"
            }
        );
    }
}
