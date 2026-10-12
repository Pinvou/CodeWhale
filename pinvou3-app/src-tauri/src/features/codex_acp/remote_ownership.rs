//! Exact ACP remote-turn custody. The registry lock fences admission and the
//! synchronous transcript start; no async wait or process shutdown runs under it.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Weak};

use anyhow::{Context, Result, bail};

use super::{AcpPool, AcpSession, RemotePermissionOwner, operation_gate};

/// A tool/elicitation ID can be reused. An old responder's post-await cleanup
/// must not erase a successor card, even on the same connection.
pub(super) fn finish_callback<T>(
    pending: &mut HashMap<String, T>,
    key: &str,
    callback: &Arc<()>,
    identity_of: impl Fn(&T) -> &Arc<()>,
    finish: impl FnOnce(),
) -> bool {
    super::lifecycle::finish_owned_callback(
        pending,
        key,
        |request| Arc::ptr_eq(identity_of(request), callback),
        finish,
    )
}

#[derive(Default)]
pub(super) struct RuntimeIdentity {
    pub(super) retired: AtomicBool,
    flights: AtomicUsize,
}

impl RuntimeIdentity {
    pub(super) fn has_live_prompt(&self) -> bool {
        self.flights.load(Ordering::Acquire) != 0
    }
}

pub(super) struct PromptFlight(Arc<RuntimeIdentity>);

impl PromptFlight {
    pub(super) fn new(identity: &Arc<RuntimeIdentity>) -> Self {
        identity.flights.fetch_add(1, Ordering::AcqRel);
        Self(Arc::clone(identity))
    }
}

impl Drop for PromptFlight {
    fn drop(&mut self) {
        self.0.flights.fetch_sub(1, Ordering::AcqRel);
    }
}

enum Phase {
    Reserved,
    Submitted(String),
    Closing(Option<String>),
}

struct Binding {
    owner: RemotePermissionOwner,
    identity: Arc<RuntimeIdentity>,
    runtime: Weak<AcpSession>,
    phase: Phase,
}

#[derive(Default)]
pub(super) struct RemoteTurnRegistry(HashMap<String, Binding>);

impl RemoteTurnRegistry {
    pub(super) fn has_custody(&self, session_id: &str) -> bool {
        self.0.contains_key(session_id)
    }

    pub(super) fn check_runtime_access(
        &self,
        session_id: &str,
        current: Option<&Arc<AcpSession>>,
    ) -> Result<()> {
        // With no custody, get_or_spawn must be allowed to reclaim a retired
        // entry. Its reusable predicate rejects the identity before spawning.
        if let Some(binding) = self.0.get(session_id) {
            if matches!(binding.phase, Phase::Closing(_))
                || binding.identity.retired.load(Ordering::Acquire)
                || !current.is_some_and(|runtime| {
                    binding
                        .runtime
                        .upgrade()
                        .is_some_and(|owned| Arc::ptr_eq(runtime, &owned))
                        && runtime.lifecycle.is_running()
                        && !runtime.restart_requested.load(Ordering::Acquire)
                })
            {
                bail!("remote_turn_custody_active");
            }
        }
        Ok(())
    }
    #[cfg(test)]
    pub(super) fn test_reserved(
        owner: RemotePermissionOwner,
        identity: &Arc<RuntimeIdentity>,
    ) -> Self {
        let mut registry = Self::default();
        registry
            .reserve(
                owner,
                identity,
                Weak::new(),
                &AtomicBool::new(false),
                &AtomicBool::new(false),
            )
            .unwrap();
        registry
    }
    // Both real local admission and remote reservation use this same gate.
    // An ACP watchdog can release busy while its old SDK request stays alive:
    // flights, not busy alone, proves remote can safely borrow the connection.
    pub(super) fn admit_local(
        &self,
        session_id: &str,
        identity: &Arc<RuntimeIdentity>,
        busy: &AtomicBool,
        configuring: &AtomicBool,
    ) -> Result<PromptFlight> {
        if self.0.contains_key(session_id) || identity.retired.load(Ordering::Acquire) {
            bail!("remote_turn_custody_active");
        }
        operation_gate::begin_prompt(busy, configuring)?;
        Ok(PromptFlight::new(identity))
    }

    fn reserve(
        &mut self,
        owner: RemotePermissionOwner,
        identity: &Arc<RuntimeIdentity>,
        runtime: Weak<AcpSession>,
        busy: &AtomicBool,
        configuring: &AtomicBool,
    ) -> Result<()> {
        if self.0.contains_key(&owner.session_id)
            || identity.retired.load(Ordering::Acquire)
            || identity.flights.load(Ordering::Acquire) != 0
        {
            bail!("remote_turn_runtime_not_idle");
        }
        operation_gate::begin_prompt(busy, configuring)?;
        self.0.insert(
            owner.session_id.clone(),
            Binding {
                owner,
                identity: Arc::clone(identity),
                runtime,
                phase: Phase::Reserved,
            },
        );
        Ok(())
    }

    pub(super) fn callback_owner(
        &self,
        session_id: &str,
        identity: &Arc<RuntimeIdentity>,
    ) -> Result<Option<RemotePermissionOwner>> {
        if identity.retired.load(Ordering::Acquire) {
            bail!("remote_turn_runtime_retired");
        }
        let Some(binding) = self.0.get(session_id) else {
            return Ok(None);
        };
        if !Arc::ptr_eq(&binding.identity, identity) {
            bail!("remote_turn_runtime_stale");
        }
        let Phase::Submitted(turn_id) = &binding.phase else {
            bail!("remote_turn_not_submitted");
        };
        let runtime = binding
            .runtime
            .upgrade()
            .context("remote_turn_runtime_stale")?;
        runtime
            .bridge
            .with_current_turn(turn_id, || Some(binding.owner.clone()))
            .context("remote_turn_not_active")
    }

    pub(super) fn active(&self, owner: &RemotePermissionOwner) -> bool {
        self.0.get(&owner.session_id).is_some_and(|binding| {
            binding.owner == *owner
                && self
                    .callback_owner(&owner.session_id, &binding.identity)
                    .is_ok_and(|current| current.as_ref() == Some(owner))
        })
    }

    pub(super) fn active_runtime(
        &self,
        owner: &RemotePermissionOwner,
    ) -> Option<(Arc<AcpSession>, String)> {
        if !self.active(owner) {
            return None;
        }
        let binding = self.0.get(&owner.session_id)?;
        let Phase::Submitted(turn) = &binding.phase else {
            return None;
        };
        Some((binding.runtime.upgrade()?, turn.clone()))
    }

    fn bind(
        &mut self,
        owner: &RemotePermissionOwner,
        identity: &Arc<RuntimeIdentity>,
        turn: &str,
        cancel: &tokio_util::sync::CancellationToken,
    ) -> Option<PromptFlight> {
        let binding = self.0.get_mut(&owner.session_id)?;
        if cancel.is_cancelled()
            || binding.owner != *owner
            || !Arc::ptr_eq(&binding.identity, identity)
            || !matches!(binding.phase, Phase::Reserved)
            || identity.retired.load(Ordering::Acquire)
        {
            return None;
        }
        binding.phase = Phase::Submitted(turn.to_owned());
        Some(PromptFlight::new(identity))
    }

    fn rollback_reserved(
        &mut self,
        owner: &RemotePermissionOwner,
        identity: &Arc<RuntimeIdentity>,
        busy: &AtomicBool,
    ) {
        if self
            .0
            .get(&owner.session_id)
            .is_some_and(|binding| matches!(binding.phase, Phase::Reserved))
            && self.remove_exact(owner, identity)
        {
            busy.store(false, Ordering::Release);
        }
    }

    pub(super) fn close(
        &mut self,
        owner: &RemotePermissionOwner,
    ) -> Option<(Arc<AcpSession>, Option<String>)> {
        let binding = self.0.get_mut(&owner.session_id)?;
        if binding.owner != *owner {
            return None;
        }
        let runtime = binding.runtime.upgrade()?;
        let turn = match &binding.phase {
            Phase::Reserved => None,
            Phase::Submitted(turn) => Some(turn.clone()),
            Phase::Closing(turn) => turn.clone(),
        };
        binding.phase = Phase::Closing(turn.clone());
        Some((runtime, turn))
    }

    pub(super) fn remove_exact(
        &mut self,
        owner: &RemotePermissionOwner,
        identity: &Arc<RuntimeIdentity>,
    ) -> bool {
        if self.0.get(&owner.session_id).is_some_and(|binding| {
            binding.owner == *owner && Arc::ptr_eq(&binding.identity, identity)
        }) {
            self.0.remove(&owner.session_id);
            true
        } else {
            false
        }
    }
}

/// One-shot preflight reservation. Reserved drop rolls back admission without
/// creating a transcript/timing turn. Submitted drop starts exact asynchronous
/// cleanup; it never reopens local admission while the old connection is live.
pub(super) struct RemoteTurnReservation {
    pool: AcpPool,
    runtime: Arc<AcpSession>,
    owner: RemotePermissionOwner,
    submitted: bool,
    released: bool,
    turn_id: String,
    permission_mode: crate::core::mode_state::SerializableMode,
}

impl AcpPool {
    pub(super) async fn reserve_remote_turn(
        &self,
        owner: RemotePermissionOwner,
        permission_mode: crate::core::mode_state::SerializableMode,
    ) -> Result<RemoteTurnReservation> {
        if self.backend(&owner.session_id) != super::AgentBackend::CodexAcp
            || !self.resolve_adapter().is_some_and(|adapter| {
                super::install::supports_remote_permission_contract(&adapter)
            })
        {
            bail!("task_permission_mode_unsupported");
        }
        verify_frozen_remote_mode(&self.session_store, &owner.session_id, permission_mode)?;
        let runtime = self.get_or_spawn(&owner.session_id).await?;
        let admission = super::PromptAdmissionGuard::new(&runtime.prompt_pending);
        let mut configuration = Some(
            self.configure_remote_permission_mode(&owner.session_id, &runtime, permission_mode)
                .await?,
        );
        let sessions = self.sessions.lock().await;
        if !sessions
            .get(&owner.session_id)
            .is_some_and(|current| Arc::ptr_eq(current, &runtime))
        {
            bail!("remote_turn_runtime_stale");
        }
        let mut owners = self.remote_permission_owners.lock();
        runtime
            .bridge
            .with_active_runtime(|| -> Result<()> {
                if !runtime.lifecycle.is_running()
                    || runtime.restart_requested.load(Ordering::Acquire)
                {
                    bail!("remote_turn_runtime_retired");
                }
                // Configuration ACK -> idle reservation is atomic under the
                // captured runtime's lifecycle gate. Interactive defaults stay unchanged.
                reserve_configured_remote_turn(
                    &mut owners,
                    owner.clone(),
                    &runtime.identity,
                    Arc::downgrade(&runtime),
                    &runtime.busy,
                    &runtime.configuring,
                    configuration.take().expect("remote configuration guard"),
                    || {
                        verify_frozen_remote_mode(
                            &self.session_store,
                            &owner.session_id,
                            permission_mode,
                        )?;
                        operation_gate::verify_remote_mode_ack(
                            &runtime.config_options.read(),
                            crate::features::remote_control::task_permissions::codex_mode(
                                permission_mode,
                            ),
                        )
                    },
                )
            })
            .context("remote_turn_runtime_retired")??;
        drop(owners);
        drop(sessions);
        drop(configuration);
        drop(admission);
        Ok(RemoteTurnReservation {
            pool: self.clone(),
            runtime,
            owner,
            submitted: false,
            released: false,
            turn_id: format!("remote-turn-{}", uuid::Uuid::new_v4().simple()),
            permission_mode,
        })
    }

    pub(super) async fn cancel_remote_turn(&self, owner: &RemotePermissionOwner) {
        // Copy the exact Arc, not a session lookup after a yield. Closing blocks
        // all new callback/approval/local admission before responder cleanup.
        let closed = { self.remote_permission_owners.lock().close(owner) };
        if let Some((runtime, Some(turn))) = closed {
            runtime.bridge.with_current_turn(&turn, || runtime.cancel());
            self.schedule_cancel_settle(owner.session_id.clone(), runtime, turn);
        }
    }
}

impl RemoteTurnReservation {
    pub(super) fn turn_id(&self) -> &str {
        &self.turn_id
    }

    pub(super) fn submit(
        &mut self,
        content: &str,
        display_content: &str,
        attachments: &[crate::features::files::file_ingest::IngestResult],
        cancel: &tokio_util::sync::CancellationToken,
        budget: crate::features::remote_control::controller::turn_budget::TurnBudget,
    ) -> Result<()> {
        verify_frozen_remote_mode(
            &self.pool.session_store,
            &self.owner.session_id,
            self.permission_mode,
        )?;
        let prepared = super::prepare_codex_prompt(
            content,
            attachments,
            &[],
            &self.runtime.prompt_capabilities,
        )?;
        self.pool
            .session_store
            .touch_activity(&self.owner.session_id)
            .context("更新 ACP 会话最近活跃时间失败")?;
        // No await from binding through transcript registration and spawn.
        // callback_owner additionally requires the exact *actual* bridge turn,
        // so binding alone cannot expose a not-yet-started approval owner.
        let flight = {
            let mut owners = self.pool.remote_permission_owners.lock();
            self.runtime
                .bridge
                .with_active_runtime(|| -> Result<_> {
                    budget.checkpoint(cancel).map_err(anyhow::Error::msg)?;
                    verify_frozen_remote_mode(
                        &self.pool.session_store,
                        &self.owner.session_id,
                        self.permission_mode,
                    )?;
                    operation_gate::verify_remote_mode_ack(
                        &self.runtime.config_options.read(),
                        crate::features::remote_control::task_permissions::codex_mode(
                            self.permission_mode,
                        ),
                    )?;
                    let flight = owners
                        .bind(&self.owner, &self.runtime.identity, &self.turn_id, cancel)
                        .context("remote_turn_reservation_stale")?;
                    self.submitted = true;
                    crate::features::assistant::timing::start_turn(&self.owner.session_id);
                    self.runtime.bridge.begin_turn_with_id(
                        self.turn_id.clone(),
                        display_content,
                        &prepared.display_attachments,
                    );
                    super::stall::mark_activity(&self.runtime.activity);
                    Ok(flight)
                })
                .context("remote_turn_runtime_retired")??
        };
        let turn_id = self.turn_id.clone();
        let runtime = Arc::clone(&self.runtime);
        let pool = self.pool.clone();
        tokio::spawn(async move {
            let _flight = flight;
            if Arc::clone(&runtime).prompt(turn_id, prepared.blocks).await {
                // The captured-Arc gate also fences global upgrade state.
                pool.handle_outdated_codex_runtime(runtime).await;
            }
        });
        Ok(())
    }

    pub(super) fn is_active(&self) -> bool {
        super::remote_turn::OwnedAcpTurn::new(Arc::clone(&self.runtime), self.turn_id.clone())
            .is_active()
            && self
                .pool
                .remote_permission_owners
                .lock()
                .active(&self.owner)
    }

    pub(super) async fn retire(&self, reason: &str) {
        super::remote_turn::OwnedAcpTurn::new(Arc::clone(&self.runtime), self.turn_id.clone())
            .retire(&self.pool, reason)
            .await;
    }

    pub(super) async fn release(mut self) {
        self.pool.release_remote_permission_owner(&self.owner).await;
        self.released = true;
    }
}

fn verify_frozen_remote_mode(
    store: &crate::features::sessions::SessionStore,
    session_id: &str,
    expected: crate::core::mode_state::SerializableMode,
) -> Result<()> {
    let current =
        crate::features::remote_control::task_permissions::for_session(store, session_id, true)
            .map_err(anyhow::Error::msg)?;
    if current != expected {
        bail!("task_permission_mode_changed");
    }
    Ok(())
}

/// Called under the captured bridge lifecycle gate and owner registry lock.
/// Consume the ACK/configuration slot only after the final policy validation;
/// no await permits a local configuration or prompt between these transitions.
fn reserve_configured_remote_turn(
    registry: &mut RemoteTurnRegistry,
    owner: RemotePermissionOwner,
    identity: &Arc<RuntimeIdentity>,
    runtime: Weak<AcpSession>,
    busy: &AtomicBool,
    configuring: &AtomicBool,
    configuration: operation_gate::ConfigurationGuard<'_>,
    validate: impl FnOnce() -> Result<()>,
) -> Result<()> {
    validate()?;
    drop(configuration);
    registry.reserve(owner, identity, runtime, busy, configuring)
}

impl Drop for RemoteTurnReservation {
    fn drop(&mut self) {
        if self.released {
            return;
        }
        if !self.submitted {
            self.pool.remote_permission_owners.lock().rollback_reserved(
                &self.owner,
                &self.runtime.identity,
                &self.runtime.busy,
            );
        } else {
            let pool = self.pool.clone();
            let owner = self.owner.clone();
            let runtime = Arc::clone(&self.runtime);
            // Future cancellation/panic must not leave a remote permission
            // callback classified as local. Retired is checked by the actual
            // connection callbacks even after its registry entry is removed.
            self.runtime.identity.retired.store(true, Ordering::Release);
            self.runtime
                .seal_remote_turn(&self.turn_id, "remote_turn_dropped");
            if let Ok(handle) = tokio::runtime::Handle::try_current() {
                handle.spawn(async move {
                    let _runtime = runtime;
                    pool.cancel_remote_turn(&owner).await;
                    pool.release_remote_permission_owner(&owner).await;
                });
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stale_callback_cleanup_cannot_erase_a_reused_tool_or_elicitation_id() {
        let finishes = std::cell::Cell::new(0);
        let old = Arc::new(());
        let successor = Arc::new(());
        let mut pending = HashMap::from([("reused-id".to_owned(), Arc::clone(&successor))]);
        assert!(!finish_callback(
            &mut pending,
            "reused-id",
            &old,
            |request| request,
            || finishes.set(finishes.get() + 1)
        ));
        assert!(Arc::ptr_eq(&pending["reused-id"], &successor));
        assert_eq!(
            finishes.get(),
            0,
            "late callbacks cannot reset successor activity"
        );
        assert!(finish_callback(
            &mut pending,
            "reused-id",
            &successor,
            |request| request,
            || finishes.set(finishes.get() + 1)
        ));
        assert!(pending.is_empty());
        assert_eq!(finishes.get(), 1);
    }

    fn owner(task: &str) -> RemotePermissionOwner {
        RemotePermissionOwner {
            task_sequence: 1,
            session_id: "session-custody".into(),
            task_id: task.into(),
            lease: crate::features::remote_control::controller::protocol::LeaseContext {
                connection_id: "connection-custody".into(),
                controller_lease_id: "lease-custody".into(),
                lease_epoch: "1".into(),
                relay_epoch: "relay-custody".into(),
                remote_control_generation: "generation-custody".into(),
            },
        }
    }

    #[test]
    fn configured_remote_admission_rejects_bad_ack_without_custody_or_prompt() {
        for current in ["agent-full-access", "read-only", "agent"] {
            let mut registry = RemoteTurnRegistry::default();
            let identity = Arc::new(RuntimeIdentity::default());
            let busy = AtomicBool::new(false);
            let configuring = AtomicBool::new(false);
            let configuration =
                operation_gate::begin_configuration(&busy, &configuring, "busy").unwrap();
            let options = vec![serde_json::from_value::<super::super::SessionConfigOption>(serde_json::json!({
                "id":"mode", "name":"Mode", "type":"select", "currentValue":current,
                "options":[{"value":"read-only","name":"Read-only"},{"value":"agent","name":"Agent"},{"value":"agent-full-access","name":"Full access"}]
            })).unwrap()];
            let selected = owner("remote-configured");
            let result = reserve_configured_remote_turn(
                &mut registry,
                selected.clone(),
                &identity,
                Weak::new(),
                &busy,
                &configuring,
                configuration,
                || operation_gate::verify_remote_mode_ack(&options, "agent"),
            );
            assert!(!configuring.load(Ordering::Acquire));
            if current == "agent" {
                result.unwrap();
                assert!(busy.load(Ordering::Acquire));
                assert!(registry.has_custody(&selected.session_id));
                assert!(
                    registry
                        .admit_local(&selected.session_id, &identity, &busy, &configuring)
                        .is_err()
                );
                registry.rollback_reserved(&selected, &identity, &busy);
            } else {
                assert_eq!(
                    result.unwrap_err().to_string(),
                    "task_permission_mode_unsupported"
                );
                assert!(!busy.load(Ordering::Acquire));
                assert!(registry.0.is_empty());
                assert!(
                    registry
                        .bind(
                            &selected,
                            &identity,
                            "never-started",
                            &tokio_util::sync::CancellationToken::new()
                        )
                        .is_none()
                );
            }
            assert!(!identity.has_live_prompt());
        }
    }

    #[test]
    fn policy_change_during_configuration_rolls_back_before_remote_admission() {
        let mut registry = RemoteTurnRegistry::default();
        let identity = Arc::new(RuntimeIdentity::default());
        let busy = AtomicBool::new(false);
        let configuring = AtomicBool::new(false);
        let configuration =
            operation_gate::begin_configuration(&busy, &configuring, "busy").unwrap();
        let selected = owner("remote-configured");
        let error = reserve_configured_remote_turn(
            &mut registry,
            selected,
            &identity,
            Weak::new(),
            &busy,
            &configuring,
            configuration,
            || bail!("task_permission_mode_changed"),
        )
        .unwrap_err();
        assert_eq!(error.to_string(), "task_permission_mode_changed");
        assert!(!configuring.load(Ordering::Acquire));
        assert!(!busy.load(Ordering::Acquire));
        assert!(registry.0.is_empty());
        assert!(!identity.has_live_prompt());
    }

    #[test]
    fn busy_local_turn_cannot_become_a_remote_permission_owner() {
        let mut registry = RemoteTurnRegistry::default();
        let identity = Arc::new(RuntimeIdentity::default());
        let busy = AtomicBool::new(true);
        let owner = owner("remote");
        assert!(
            registry
                .reserve(
                    owner.clone(),
                    &identity,
                    Weak::new(),
                    &busy,
                    &AtomicBool::new(false)
                )
                .is_err()
        );
        assert!(registry.0.is_empty());
        assert!(
            registry
                .callback_owner(&owner.session_id, &identity)
                .unwrap()
                .is_none()
        );
        assert!(
            busy.load(Ordering::Acquire),
            "failed remote admission must not clear local busy"
        );
    }

    #[test]
    fn locally_settled_sdk_request_still_blocks_remote_reuse_until_actual_drop() {
        let mut registry = RemoteTurnRegistry::default();
        let identity = Arc::new(RuntimeIdentity::default());
        let busy = AtomicBool::new(false);
        let configuring = AtomicBool::new(false);
        let owner = owner("remote");
        let flight = registry
            .admit_local(&owner.session_id, &identity, &busy, &configuring)
            .unwrap();
        busy.store(false, Ordering::Release); // Stop/watchdog settled, SDK request still alive.
        assert!(
            registry
                .reserve(owner.clone(), &identity, Weak::new(), &busy, &configuring)
                .is_err()
        );
        assert!(!busy.load(Ordering::Acquire));
        drop(flight); // The real spawned prompt future owns this same RAII counter.
        registry
            .reserve(owner.clone(), &identity, Weak::new(), &busy, &configuring)
            .unwrap();
        assert!(busy.load(Ordering::Acquire));
        registry.rollback_reserved(&owner, &identity, &busy);
        assert!(!busy.load(Ordering::Acquire));
    }

    #[test]
    fn reserved_and_submitted_custody_block_local_admission_even_after_busy_clears() {
        let identity = Arc::new(RuntimeIdentity::default());
        let owner = owner("remote");
        let mut registry = RemoteTurnRegistry::test_reserved(owner.clone(), &identity);
        let busy = AtomicBool::new(false);
        let config = AtomicBool::new(false);
        assert!(
            registry
                .admit_local(&owner.session_id, &identity, &busy, &config)
                .is_err()
        );
        assert!(
            registry
                .callback_owner(&owner.session_id, &identity)
                .is_err(),
            "Reserved is never a permission owner"
        );
        let flight = registry
            .bind(
                &owner,
                &identity,
                "remote-turn-exact",
                &tokio_util::sync::CancellationToken::new(),
            )
            .unwrap();
        assert!(
            registry
                .bind(
                    &owner,
                    &identity,
                    "remote-turn-retry",
                    &tokio_util::sync::CancellationToken::new()
                )
                .is_none(),
            "one-shot submit"
        );
        assert!(
            registry
                .admit_local(&owner.session_id, &identity, &busy, &config)
                .is_err()
        );
        assert!(
            registry
                .callback_owner(&owner.session_id, &identity)
                .is_err(),
            "no actual runtime/bridge is not active evidence"
        );
        drop(flight);
    }

    #[test]
    fn retired_and_foreign_connection_callbacks_never_fall_back_to_local() {
        let identity = Arc::new(RuntimeIdentity::default());
        let successor = Arc::new(RuntimeIdentity::default());
        let owner = owner("remote");
        let mut registry = RemoteTurnRegistry::test_reserved(owner.clone(), &identity);
        let flight = registry
            .bind(
                &owner,
                &identity,
                "remote-turn-exact",
                &tokio_util::sync::CancellationToken::new(),
            )
            .unwrap();
        assert!(
            registry
                .callback_owner(&owner.session_id, &successor)
                .is_err()
        );
        identity.retired.store(true, Ordering::Release);
        assert!(
            registry
                .callback_owner(&owner.session_id, &identity)
                .is_err()
        );
        registry.remove_exact(&owner, &identity);
        assert!(
            registry
                .check_runtime_access(&owner.session_id, None)
                .is_ok(),
            "released custody must permit dead-entry reclamation"
        );
        assert!(
            registry
                .callback_owner(&owner.session_id, &identity)
                .is_err(),
            "retirement survives registry removal"
        );
        assert!(
            registry
                .callback_owner(&owner.session_id, &successor)
                .unwrap()
                .is_none()
        );
        drop(flight);
    }

    #[test]
    fn stale_owner_and_runtime_cleanup_cannot_remove_a_successor() {
        let identity = Arc::new(RuntimeIdentity::default());
        let successor = Arc::new(RuntimeIdentity::default());
        let old = owner("old");
        let current = owner("current");
        let mut registry = RemoteTurnRegistry::test_reserved(current.clone(), &successor);
        let busy = AtomicBool::new(true);
        assert!(!registry.remove_exact(&old, &identity));
        assert!(
            !registry.remove_exact(&current, &identity),
            "even same tuple cannot replace connection identity"
        );
        registry.rollback_reserved(&old, &identity, &busy);
        assert!(busy.load(Ordering::Acquire));
        assert!(registry.0.contains_key(&current.session_id));
        assert!(registry.close(&old).is_none());
    }

    #[test]
    fn preflight_rollback_releases_only_its_reserved_busy_slot() {
        let identity = Arc::new(RuntimeIdentity::default());
        let owner = owner("remote");
        let mut registry = RemoteTurnRegistry::test_reserved(owner.clone(), &identity);
        let busy = AtomicBool::new(true);
        registry.rollback_reserved(&owner, &identity, &busy);
        assert!(registry.0.is_empty());
        assert!(!busy.load(Ordering::Acquire));
        let flight = registry
            .admit_local(&owner.session_id, &identity, &busy, &AtomicBool::new(false))
            .unwrap();
        registry.rollback_reserved(&owner, &identity, &busy);
        assert!(
            busy.load(Ordering::Acquire),
            "a repeated old rollback must not clear a later local admission"
        );
        drop(flight);
    }

    #[test]
    fn closing_custody_rejects_new_approvals_and_local_admission() {
        let identity = Arc::new(RuntimeIdentity::default());
        let owner = owner("remote");
        let mut registry = RemoteTurnRegistry::test_reserved(owner.clone(), &identity);
        registry.0.get_mut(&owner.session_id).unwrap().phase =
            Phase::Closing(Some("remote-turn-exact".into()));
        assert!(
            registry
                .callback_owner(&owner.session_id, &identity)
                .is_err()
        );
        assert!(!registry.active(&owner));
        assert!(
            registry
                .check_runtime_access(&owner.session_id, None)
                .is_err(),
            "no respawn during journal handoff"
        );
        assert!(
            registry
                .admit_local(
                    &owner.session_id,
                    &identity,
                    &AtomicBool::new(false),
                    &AtomicBool::new(false)
                )
                .is_err()
        );
    }

    #[test]
    fn cancelled_preflight_cannot_submit_or_allocate_a_prompt_flight() {
        let identity = Arc::new(RuntimeIdentity::default());
        let owner = owner("remote");
        let mut registry = RemoteTurnRegistry::test_reserved(owner.clone(), &identity);
        let busy = AtomicBool::new(true);
        let cancel = tokio_util::sync::CancellationToken::new();
        cancel.cancel();
        assert!(
            registry
                .bind(&owner, &identity, "must-not-start", &cancel)
                .is_none()
        );
        assert_eq!(identity.flights.load(Ordering::Acquire), 0);
        assert!(matches!(
            registry.0[&owner.session_id].phase,
            Phase::Reserved
        ));
        registry.rollback_reserved(&owner, &identity, &busy);
        assert!(!busy.load(Ordering::Acquire));
        assert!(registry.0.is_empty());
    }

    #[test]
    fn missing_runtime_cannot_respawn_under_any_remote_custody() {
        let identity = Arc::new(RuntimeIdentity::default());
        let owner = owner("remote");
        let mut registry = RemoteTurnRegistry::test_reserved(owner.clone(), &identity);
        assert!(
            registry
                .check_runtime_access(&owner.session_id, None)
                .is_err()
        );
        let flight = registry
            .bind(
                &owner,
                &identity,
                "remote-turn-exact",
                &tokio_util::sync::CancellationToken::new(),
            )
            .unwrap();
        assert!(
            registry
                .check_runtime_access(&owner.session_id, None)
                .is_err()
        );
        registry.remove_exact(&owner, &identity);
        assert!(
            registry
                .check_runtime_access(&owner.session_id, None)
                .is_ok()
        );
        drop(flight);
    }
}
