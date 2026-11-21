//! Journal facts belong to the captured turn, not the ACP pending-map lifetime.
use std::collections::{HashMap, HashSet};

use super::{
    ControllerTaskEventSink, JournalKind, PermissionAuthority, RemotePermissionOwner,
    RemoteTaskService, TaskLeaseBinding, json,
};
use crate::features::remote_control::controller::permission::PermissionView;

pub(super) struct PermissionJournal {
    owner: RemotePermissionOwner,
    registered: HashMap<String, String>,
    projected: HashSet<String>,
}

impl PermissionJournal {
    pub(super) fn new(owner: RemotePermissionOwner) -> Self {
        Self {
            owner,
            registered: HashMap::new(),
            projected: HashSet::new(),
        }
    }

    pub(super) fn approval(&self, callback: &str) -> Option<&str> {
        self.registered.get(callback).map(String::as_str)
    }

    /// Completion is a retained fact, not only an event in the current poll.
    /// An empty journal cannot authorize resuming a waiting task.
    pub(super) fn all_registered_projected(&self) -> bool {
        !self.registered.is_empty()
            && self
                .registered
                .values()
                .all(|approval| self.projected.contains(approval))
    }

    pub(super) fn register(
        &mut self,
        callback: &str,
        view: &PermissionView,
        events: &dyn ControllerTaskEventSink,
    ) -> Result<(), String> {
        if view.task_id != self.owner.task_id || self.registered.contains_key(callback) {
            return Err("permission_binding_stale".into());
        }
        self.registered
            .insert(callback.to_owned(), view.permission_request_id.clone());
        events.emit_journal(
            JournalKind::Permission,
            &self.owner.session_id,
            Some(&self.owner.task_id),
            json!({ "status":"pending", "operation_type":view.operation_type,
                "risk_level":view.risk_level, "target_scope":view.target_scope }),
        );
        Ok(())
    }

    pub(super) fn project_missing(
        &mut self,
        authority: &PermissionAuthority,
        tasks: &RemoteTaskService,
        events: &dyn ControllerTaskEventSink,
        current: &HashSet<String>,
    ) -> Result<usize, String> {
        self.project(authority, tasks, events, |callback| {
            !current.contains(callback)
        })
    }

    /// No ACP calls, new approvals, clock reads, or task-state transitions.
    /// Use at both turn exit and after the existing exact-owner pending denial.
    pub(super) fn project_all(
        &mut self,
        authority: &PermissionAuthority,
        tasks: &RemoteTaskService,
        events: &dyn ControllerTaskEventSink,
    ) -> Result<usize, String> {
        self.project(authority, tasks, events, |_| true)
    }

    fn project(
        &mut self,
        authority: &PermissionAuthority,
        tasks: &RemoteTaskService,
        events: &dyn ControllerTaskEventSink,
        eligible: impl Fn(&str) -> bool,
    ) -> Result<usize, String> {
        let record = tasks
            .status(&self.owner.task_id)
            .map_err(|e| e.code.to_owned())?;
        if record.sequence != self.owner.task_sequence
            || record.session_id != self.owner.session_id
            || record.lease != TaskLeaseBinding::from_context(&self.owner.lease)
        {
            return Err("permission_binding_stale".into());
        }
        if record.is_terminal() {
            return Err("permission_task_terminal".into());
        }
        let mut count = 0;
        for (callback, approval) in &self.registered {
            if !eligible(callback) || self.projected.contains(approval) {
                continue;
            }
            let view = authority
                .view_for_binding(&self.owner.lease, &self.owner.task_id, approval)
                .map_err(|e| e.code.to_owned())?;
            if view.state == "pending" {
                continue;
            }
            // An uncommitted Allow may still lose to B stop. Never project it
            // as the final result before the authority's dispatch boundary.
            if view.state == "allowed" && view.dispatch_state != "dispatched" {
                continue;
            }
            events.emit_journal(
                JournalKind::Permission,
                &self.owner.session_id,
                Some(&self.owner.task_id),
                json!({ "status":view.state, "operation_type":view.operation_type,
                    "risk_level":view.risk_level, "target_scope":view.target_scope,
                    "decision":view.decision }),
            );
            self.projected.insert(approval.clone());
            count += 1;
        }
        Ok(count)
    }
}
