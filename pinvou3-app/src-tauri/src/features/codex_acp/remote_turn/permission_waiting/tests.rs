use super::*;
use crate::features::codex_acp::{
    CodexAcpPendingPermission, PendingPermission, RemotePermissionProjection,
    mint_remote_permission_in_maps, remote_permission_response_key,
};
use crate::features::remote_control::controller::{
    permission::{DecisionAuthority, PermissionDecision},
    protocol::LeaseContext,
    task::{RemoteTaskLedger, RemoteTaskRequest, RemoteTaskTerminal, task_input_digest_for_value},
};
use serde_json::{Value, json};
use std::sync::atomic::{AtomicU64, Ordering};
use tokio::sync::{Mutex, oneshot};

// Only provider/runtime activation and response delivery are controlled. The
// production polling loop, B authority, pool mint, owner matching and task
// ledger/state service below are real; the one-active-per-task cap is retained.
struct Pool {
    pending: Mutex<HashMap<String, PendingPermission>>,
    owners: parking_lot::Mutex<HashMap<String, RemotePermissionOwner>>,
}
impl Pool {
    async fn insert(&self, owner: &RemotePermissionOwner, callback: &str) {
        let (response_tx, _) = oneshot::channel();
        self.pending.lock().await.insert(
            callback.into(),
            PendingPermission {
                callback_instance: Arc::new(()),
                view: CodexAcpPendingPermission {
                    session_id: owner.session_id.clone(),
                    tool_call_id: callback.into(),
                    request: Value::Null,
                },
                option_ids: vec![],
                option_kinds: HashMap::new(),
                generation: 1,
                remote_owner: Some(owner.clone()),
                remote_request_id: Some(callback.into()),
                remote_approval_id: None,
                remote_projection: Some(RemotePermissionProjection {
                    operation_type: "shell_command".into(),
                    risk_level: "high".into(),
                    target_scope: "[PATH]".into(),
                    preview: "[REDACTED]".into(),
                }),
                turn_id: None,
                activity: crate::features::codex_acp::stall::new_activity_clock(),
                response_tx,
                bridge: None,
            },
        );
    }
}
#[async_trait::async_trait]
impl PermissionPool for Pool {
    async fn pending_remote_permissions(
        &self,
        owner: &RemotePermissionOwner,
    ) -> Vec<RemoteOwnedPermission> {
        self.pending
            .lock()
            .await
            .values()
            .filter_map(|p| {
                if p.remote_owner.as_ref() != Some(owner) {
                    return None;
                }
                Some(RemoteOwnedPermission {
                    callback_id: p.remote_request_id.clone()?,
                    approval_id: p.remote_approval_id.clone(),
                    owner: owner.clone(),
                    projection: p.remote_projection.clone()?,
                })
            })
            .collect()
    }
    async fn mint_remote_permission(
        &self,
        owner: &RemotePermissionOwner,
        callback: &str,
        authority: &PermissionAuthority,
        ledger: &RemoteTaskLedger,
        ttl: u64,
    ) -> anyhow::Result<PermissionView> {
        mint_remote_permission_in_maps(
            &self.pending,
            &self.owners,
            owner,
            callback,
            authority,
            ledger,
            ttl,
        )
        .await
    }
    async fn respond_remote_permission(
        &self,
        owner: &RemotePermissionOwner,
        approval: &str,
        _: bool,
    ) -> anyhow::Result<()> {
        let mut pending = self.pending.lock().await;
        let key = remote_permission_response_key(&pending, owner, approval)
            .ok_or_else(|| anyhow::anyhow!("stale"))?;
        pending.remove(&key).unwrap();
        Ok(())
    }
}
struct Events;
impl ControllerTaskEventSink for Events {
    fn emit(&self, _: Value) {}
}
struct Fixture {
    tasks: RemoteTaskService,
    owner: RemotePermissionOwner,
    authority: PermissionAuthority,
    clock: Arc<AtomicU64>,
    pool: Pool,
    registered: HashMap<String, String>,
    resolved: HashSet<String>,
}
impl Fixture {
    fn new() -> Self {
        let lease = LeaseContext {
            connection_id: "connection-wait".into(),
            controller_lease_id: "lease-wait".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-wait".into(),
            remote_control_generation: "generation-wait".into(),
        };
        let input = json!({"text":"approval", "attachment_ids":[], "skill_ids":[], "workspace_id":"workspace-wait"});
        let request = RemoteTaskRequest::from_payload(&json!({
            "connection_id":lease.connection_id, "controller_lease_id":lease.controller_lease_id,
            "lease_epoch":lease.lease_epoch, "relay_epoch":lease.relay_epoch, "remote_control_generation":lease.remote_control_generation,
            "task_id":"task-wait", "client_request_id":"client-wait", "session_id":"session-wait",
            "capability_revision":"1", "mode":"code", "model_source":"controlled_device",
            "input_digest":task_input_digest_for_value(&input).unwrap(), "input":input,
        })).unwrap();
        let tasks = RemoteTaskService::new(Arc::new(RemoteTaskLedger::in_memory()));
        tasks.ledger().admit(&lease, &request, 1).unwrap();
        let record = tasks.ledger().begin_dispatch(&request.task_id, 2).unwrap();
        let owner = RemotePermissionOwner {
            task_sequence: record.sequence,
            session_id: record.session_id,
            task_id: record.task_id,
            lease,
        };
        let clock = Arc::new(AtomicU64::new(10));
        let read_clock = clock.clone();
        Self {
            tasks,
            authority: PermissionAuthority::with_clock(Arc::new(move || {
                read_clock.load(Ordering::SeqCst)
            })),
            pool: Pool {
                pending: Mutex::new(HashMap::new()),
                owners: parking_lot::Mutex::new(HashMap::from([(
                    owner.session_id.clone(),
                    owner.clone(),
                )])),
            },
            owner,
            clock,
            registered: HashMap::new(),
            resolved: HashSet::new(),
        }
    }
    async fn tick(&mut self) -> Result<(), String> {
        let events: Arc<dyn ControllerTaskEventSink> = Arc::new(Events);
        sync(
            &self.pool,
            &self.authority,
            &self.tasks,
            &self.owner,
            &mut self.registered,
            &mut self.resolved,
            &events,
        )
        .await
    }
    fn state(&self) -> RemoteTaskState {
        self.tasks.status(&self.owner.task_id).unwrap().state
    }
    fn decide(&self, callback: &str, decision: PermissionDecision) {
        let id = &self.registered[callback];
        self.authority
            .decide(
                DecisionAuthority::ControllerA,
                &self.owner.lease,
                &self.owner.task_id,
                id,
                decision,
                self.authority.now_ms(),
            )
            .unwrap();
        if decision == PermissionDecision::Allow {
            self.authority
                .commit_dispatch(id, self.authority.now_ms())
                .unwrap();
        }
    }
}

#[tokio::test]
async fn resolved_callback_cannot_resume_while_the_next_real_approval_waits() {
    for decision in [PermissionDecision::Allow, PermissionDecision::Deny] {
        let mut f = Fixture::new();
        f.pool.insert(&f.owner, "first").await;
        f.tick().await.unwrap();
        assert_eq!(f.state(), RemoteTaskState::WaitingPermission);
        assert_eq!(f.authority.active_count(), 1);
        f.decide("first", decision);
        // The first responder finished between polls. Only now can the next
        // callback mint under the real authority's per-task capacity rule.
        f.pool
            .respond_remote_permission(&f.owner, &f.registered["first"], true)
            .await
            .unwrap();
        f.pool.insert(&f.owner, "second").await;
        f.tick().await.unwrap();
        assert_eq!(
            f.state(),
            RemoteTaskState::WaitingPermission,
            "second is still pending"
        );
        assert_eq!(f.authority.active_count(), 1);
        assert_eq!(
            f.authority.view(&f.registered["second"]).unwrap().state,
            "pending"
        );
        f.tick().await.unwrap();
        assert_eq!(
            f.state(),
            RemoteTaskState::WaitingPermission,
            "next tick cannot lose waiting state"
        );
        f.decide("second", PermissionDecision::Deny);
        f.tick().await.unwrap();
        assert_eq!(f.state(), RemoteTaskState::Running);
        f.tick().await.unwrap();
        assert_eq!(f.state(), RemoteTaskState::Running);
    }
}

#[tokio::test]
async fn expiry_resumes_only_after_the_last_callback_and_never_revives_cancelled_task() {
    let mut f = Fixture::new();
    f.pool.insert(&f.owner, "expiry").await;
    f.tick().await.unwrap();
    let deadline = f
        .authority
        .view(&f.registered["expiry"])
        .unwrap()
        .expires_at_ms;
    f.clock.store(deadline, Ordering::SeqCst);
    f.tick().await.unwrap();
    assert_eq!(f.state(), RemoteTaskState::Running);
    f.tasks
        .enter_waiting_permission(&f.owner.task_id, f.owner.task_sequence, 5)
        .unwrap();
    f.tasks
        .ledger()
        .complete(&f.owner.task_id, RemoteTaskTerminal::Cancelled, None, 6)
        .unwrap();
    resume_when_clear(&f.tasks, &f.owner, &[]).unwrap();
    assert_eq!(f.state(), RemoteTaskState::Cancelled);
}

#[tokio::test]
async fn late_resolution_rejects_changed_lease_session_or_admission_identity() {
    let f = Fixture::new();
    f.tasks
        .enter_waiting_permission(&f.owner.task_id, f.owner.task_sequence, 3)
        .unwrap();
    for kind in 0..7 {
        let mut old = f.owner.clone();
        match kind {
            0 => old.task_sequence += 1,
            1 => old.session_id.push_str("-old"),
            2 => old.lease.connection_id.push_str("-old"),
            3 => old.lease.controller_lease_id.push_str("-old"),
            4 => old.lease.lease_epoch.push_str("-old"),
            5 => old.lease.relay_epoch.push_str("-old"),
            _ => old.lease.remote_control_generation.push_str("-old"),
        }
        assert_eq!(
            resume_when_clear(&f.tasks, &old, &[]).unwrap_err(),
            "permission_binding_stale"
        );
        assert_eq!(f.state(), RemoteTaskState::WaitingPermission);
    }
}

#[tokio::test]
async fn actual_authority_and_controller_wire_projection_preserve_each_state() {
    let mut f = Fixture::new();
    let mut views = Vec::new();
    for (callback, ending) in [
        ("p", "pending"),
        ("a", "allowed"),
        ("d", "denied"),
        ("e", "expired"),
    ] {
        // Only one active entry at a time, as in the production authority.
        f.pool.insert(&f.owner, callback).await;
        f.tick().await.unwrap();
        let id = f.registered[callback].clone();
        if ending == "allowed" {
            f.decide(callback, PermissionDecision::Allow);
        }
        if ending == "denied" {
            f.decide(callback, PermissionDecision::Deny);
        }
        if ending == "expired" {
            let deadline = f.authority.view(&id).unwrap().expires_at_ms;
            f.clock.store(deadline, Ordering::SeqCst);
            f.authority.sweep_expired(deadline);
        }
        let view = f.authority.view(&id).unwrap();
        assert_eq!(view.state, ending);
        views.push(view.into_remote_action_payload());
        if ending == "pending" {
            f.decide(callback, PermissionDecision::Deny);
        }
        f.tick().await.unwrap();
        assert!(
            f.authority.pending_for_task(&f.owner.task_id).is_empty(),
            "closed entries cannot appear in the normal list"
        );
    }
    // Browser fixture consumes these actual authority/production encoder
    // outputs. Closed-state inputs exercise defensive consumer semantics;
    // they are not evidence that the current normal list returns history.
    println!(
        "PERMISSION_PROJECTION_JSON {}",
        json!({"permissions":views})
    );
}
