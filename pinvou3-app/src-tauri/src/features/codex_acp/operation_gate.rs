use std::ops::Deref;
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};

use anyhow::{Context, Result, bail};
use serde_json::json;

use super::{AcpPool, AcpSession, CodexAcpSessionInfo, events::patch_acp_state};

/// Owns the session's configuration slot until the operation finishes. Drop
/// based release keeps cancellation and early-return paths from leaving the
/// session permanently stuck in `configuring`.
pub(super) struct ConfigurationGuard<T: Deref<Target = AtomicBool>> {
    configuring: T,
}

impl<T: Deref<Target = AtomicBool>> Drop for ConfigurationGuard<T> {
    fn drop(&mut self) {
        self.configuring.store(false, Ordering::Release);
    }
}

pub(super) fn begin_configuration<T: Deref<Target = AtomicBool>>(
    busy: &AtomicBool,
    configuring: T,
    busy_error: &str,
) -> Result<ConfigurationGuard<T>> {
    if busy.load(Ordering::Acquire) {
        bail!("{busy_error}");
    }
    if configuring.swap(true, Ordering::AcqRel) {
        bail!("ACP 会话已有配置正在同步");
    }
    if busy.load(Ordering::Acquire) {
        configuring.store(false, Ordering::Release);
        bail!("{busy_error}");
    }
    Ok(ConfigurationGuard { configuring })
}

/// Reserve the prompt slot with a final configuration recheck. Together with
/// `begin_configuration`'s final busy recheck this closes both interleavings
/// without serializing unrelated sessions behind a global mutex.
pub(super) fn begin_prompt(busy: &AtomicBool, configuring: &AtomicBool) -> Result<()> {
    if configuring.load(Ordering::Acquire) {
        bail!("ACP 会话配置仍在同步，请稍候再发送");
    }
    if busy.swap(true, Ordering::AcqRel) {
        bail!("ACP 会话仍在生成");
    }
    if configuring.load(Ordering::Acquire) {
        busy.store(false, Ordering::Release);
        bail!("ACP 会话配置仍在同步，请稍候再发送");
    }
    Ok(())
}

/// Keep the configuration admission until a non-cancellable worker finishes,
/// even if its caller stops awaiting it. Other instances keep their own slot.
async fn persist_with_configuration<T: Send + 'static>(
    configuration: Arc<ConfigurationGuard<Arc<AtomicBool>>>,
    action: impl FnOnce() -> T + Send + 'static,
) -> Result<T> {
    tokio::task::spawn_blocking(move || {
        let _configuration = configuration;
        action()
    })
    .await
    .context("ACP 配置持久化任务失败")
}

/// Admit a prompt turn atomically across the fallible activity touch.
///
/// The sequence — busy admission, activity touch, timing registration — must
/// stay atomic with respect to the touch failure: registration writes both a
/// queue entry and a persisted `user_start` event that only a spawned prompt's
/// `timing::finish_turn` can pair, so registering before a touch that then
/// fails would leak both with no task left to finish them (round-8 review).
/// Running the touch inside the admitted section (busy held) preserves the
/// admission semantics and lets the failure path roll back the slot before
/// returning.
///
/// The timing registration must also happen inside the admitted section and
/// before the prompt task is spawned: the spawned task can complete (fast mock
/// response, instant connection error) and call `timing::finish_turn` before
/// `send_message` returns, so registering from the caller after the await would
/// race that finish — the completion would find an empty queue and be dropped,
/// leaving the late registration as a stale unpaired entry. Registering only
/// after admission also guarantees a rejected concurrent submit never enqueues
/// a ghost turn whose send_error finish would clear the in-flight turn's
/// queue (round-7 review invariant).
pub(super) fn admit_prompt_turn(
    busy: &AtomicBool,
    configuring: &AtomicBool,
    session_id: &str,
    touch_activity: impl FnOnce() -> Result<()>,
) -> Result<()> {
    begin_prompt(busy, configuring)?;
    finish_prompt_admission(busy, session_id, touch_activity)
}

pub(super) fn finish_prompt_admission(
    busy: &AtomicBool,
    session_id: &str,
    touch_activity: impl FnOnce() -> Result<()>,
) -> Result<()> {
    if let Err(error) = touch_activity() {
        busy.store(false, Ordering::Release);
        return Err(error);
    }
    crate::features::assistant::timing::start_turn(session_id);
    Ok(())
}

enum SessionConfigChange<'a> {
    Model(&'a str),
    Mode(&'a str),
    Option {
        config_id: &'a str,
        value_id: &'a str,
    },
}

impl SessionConfigChange<'_> {
    fn config_id(&self) -> &str {
        match self {
            Self::Model(_) => "model",
            Self::Mode(_) => "mode",
            Self::Option { config_id, .. } => config_id,
        }
    }

    fn value_id(&self) -> &str {
        match self {
            Self::Model(value_id) | Self::Mode(value_id) => value_id,
            Self::Option { value_id, .. } => value_id,
        }
    }

    fn busy_error(&self) -> &'static str {
        match self {
            Self::Model(_) => "Agent 正在处理当前任务，模型将在本轮结束后才能修改",
            Self::Mode(_) => "Agent 正在处理当前任务，权限模式将在本轮结束后才能修改",
            Self::Option { .. } => "Agent 正在处理当前任务，配置将在本轮结束后才能修改",
        }
    }

    async fn apply(&self, runtime: &AcpSession) -> Result<()> {
        match self {
            Self::Model(model_id) => runtime.set_config_option("model", model_id).await,
            Self::Mode(mode_id) => runtime.set_mode(mode_id).await,
            Self::Option {
                config_id,
                value_id,
            } => runtime.set_config_option(config_id, value_id).await,
        }
    }
}

pub(super) fn verify_remote_mode_ack(
    options: &[super::SessionConfigOption],
    desired: &str,
) -> Result<()> {
    if !super::config_option_supports(options, "mode", desired)
        || super::current_config_value(options, "mode").as_deref() != Some(desired)
    {
        bail!("task_permission_mode_unsupported");
    }
    Ok(())
}

fn finish_remote_configuration_admission<T>(admission: Option<Result<T>>) -> Result<T> {
    // None means this precise runtime retired, not a permanent mode capability
    // failure. Preserve inner busy/configuration errors without relabeling them.
    admission.context("remote_turn_runtime_retired")?
}

fn finish_remote_mode_configuration(
    update: Result<()>,
    verify_ack: impl FnOnce() -> Result<()>,
) -> Result<()> {
    // A failed RPC does not prove a permanent capability failure. Preserve its
    // cause and do not accept an old configuration snapshot as a fresh ACK.
    update?;
    verify_ack()
}

impl AcpPool {
    /// Remote configuration is task-local and never updates interactive defaults.
    /// Only the pinned Codex ACP adapter's documented on-request modes qualify.
    pub(super) async fn configure_remote_permission_mode(
        &self,
        session_id: &str,
        runtime: &AcpSession,
        mode: crate::core::mode_state::SerializableMode,
    ) -> Result<ConfigurationGuard<Arc<AtomicBool>>> {
        if self.backend(session_id) != super::AgentBackend::CodexAcp
            || runtime.backend != super::AgentBackend::CodexAcp
            || !runtime.remote_permission_contract
        {
            bail!("task_permission_mode_unsupported");
        }
        let desired = crate::features::remote_control::task_permissions::codex_mode(mode);
        let guard = {
            let owners = self.remote_permission_owners.lock();
            finish_remote_configuration_admission(runtime.bridge.with_active_runtime(|| {
                if owners.has_custody(session_id) || runtime.identity.has_live_prompt() {
                    bail!("task_session_busy");
                }
                begin_configuration(
                    &runtime.busy,
                    runtime.configuring.clone(),
                    "task_session_busy",
                )
            }))?
        };
        if !super::config_option_supports(&runtime.config_options.read(), "mode", desired) {
            bail!("task_permission_mode_unsupported");
        }
        finish_remote_mode_configuration(runtime.set_config_option("mode", desired).await, || {
            verify_remote_mode_ack(&runtime.config_options.read(), desired)
        })?;
        Ok(guard)
    }

    async fn remember_config_choice(
        &self,
        runtime: &AcpSession,
        configuration: Arc<ConfigurationGuard<Arc<AtomicBool>>>,
        session_id: &str,
        config_id: &str,
        value_id: &str,
    ) -> Result<()> {
        let backend = self.backend(session_id);
        // Round-49 review: both store writes poll the cross-process section
        // lock (up to SECTION_LOCK_TIMEOUT) and rewrite their whole JSON
        // store, so they run on a blocking worker — the async runtime
        // workers here also drive the ACP engine session pumps.
        let agents = self.agents.clone();
        let config_defaults = self.config_defaults.clone();
        let session_owned = session_id.to_string();
        let config_owned = config_id.to_string();
        let value_owned = value_id.to_string();
        let bridge = runtime.bridge.clone();
        let errors = persist_with_configuration(configuration, move || {
            // A retired instance must not publish choices for its successor.
            // Acquire the same retirement fence on the blocking worker, not
            // on the async event pump, and hold it through both store writes.
            bridge
                .with_active_runtime(|| {
                    let mut errors = Vec::new();
                    if let Err(error) =
                        agents.set_acp_config_value(&session_owned, &config_owned, &value_owned)
                    {
                        errors.push(format!("会话配置: {error:#}"));
                    }
                    if let Err(error) = config_defaults.set(backend, &config_owned, &value_owned) {
                        errors.push(format!("新会话默认值: {error:#}"));
                    }
                    errors
                })
                .context("ACP runtime 已退休，请重试新实例")
        })
        .await??;
        if !errors.is_empty() {
            let message = errors.join("；");
            eprintln!(
                "[pinvou3-app] failed to persist {} ACP config {}={}: {}",
                backend.display_name(),
                config_id,
                value_id,
                message
            );
        }
        Ok(())
    }

    async fn apply_config_change(
        &self,
        session_id: &str,
        change: SessionConfigChange<'_>,
    ) -> Result<CodexAcpSessionInfo> {
        let runtime = self.get_or_spawn(session_id).await?;
        let configuration = Arc::new(
            runtime
                .bridge
                .with_active_runtime(|| {
                    begin_configuration(
                        &runtime.busy,
                        runtime.configuring.clone(),
                        change.busy_error(),
                    )
                })
                .context("ACP runtime 已退休，请重试新实例")??,
        );
        let config_id = change.config_id();
        let value_id = change.value_id();
        if let Err(error) = change.apply(&runtime).await {
            return Err(error);
        }
        self.remember_config_choice(
            &runtime,
            configuration.clone(),
            session_id,
            config_id,
            value_id,
        )
        .await?;
        let pending_permissions = self.pending_permissions_for(session_id).await;
        let pending_elicitations = self.pending_elicitations_for(session_id).await;
        runtime
            .bridge
            .with_active_runtime(|| -> Result<CodexAcpSessionInfo> {
                runtime.bridge.emit(
                    "config_change_applied",
                    json!({ "configId": config_id, "valueId": value_id }),
                );
                let info = runtime.info(&self.agents, pending_permissions, pending_elicitations);
                patch_acp_state(session_id, json!({ "session": &info }))?;
                Ok(info)
            })
            .context("ACP runtime 已退休，请重试新实例")?
    }

    pub async fn set_model(&self, session_id: &str, model_id: &str) -> Result<CodexAcpSessionInfo> {
        self.apply_config_change(session_id, SessionConfigChange::Model(model_id))
            .await
    }

    pub async fn set_config_option(
        &self,
        session_id: &str,
        config_id: &str,
        value_id: &str,
    ) -> Result<CodexAcpSessionInfo> {
        self.apply_config_change(
            session_id,
            SessionConfigChange::Option {
                config_id,
                value_id,
            },
        )
        .await
    }

    pub async fn set_mode(&self, session_id: &str, mode_id: &str) -> Result<CodexAcpSessionInfo> {
        self.apply_config_change(session_id, SessionConfigChange::Mode(mode_id))
            .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn failed_remote_mode_update_preserves_cause_and_never_accepts_stale_ack() {
        for cause in [
            "config_transport_closed",
            "agent_config_rejected",
            "remote_turn_runtime_retired",
        ] {
            let ack_checks = std::cell::Cell::new(0);
            let error = finish_remote_mode_configuration(
                Err(anyhow::anyhow!(cause).context("ACP session/set_config_option 失败")),
                || {
                    ack_checks.set(ack_checks.get() + 1);
                    Ok(())
                },
            )
            .unwrap_err();
            assert_eq!(ack_checks.get(), 0);
            assert_eq!(error.root_cause().to_string(), cause);
            assert_eq!(error.to_string(), "ACP session/set_config_option 失败");
            assert!(
                !error
                    .chain()
                    .any(|item| item.to_string() == "task_permission_mode_unsupported")
            );
            assert_eq!(
                crate::features::assistant::remote_turn::remote_engine_error_code(&error),
                "task_engine_unavailable"
            );
        }
        let ack_error =
            finish_remote_mode_configuration(Ok(()), || verify_remote_mode_ack(&[], "agent"))
                .unwrap_err();
        assert_eq!(ack_error.to_string(), "task_permission_mode_unsupported");
    }

    #[tokio::test]
    async fn cancelled_configuration_keeps_slot_until_blocking_persistence_finishes() {
        use super::super::store::SessionAgentStore;
        let (_lock, _env) = crate::platform::test_support::locked_env(&["PINVOU3_HOME"]);
        let tmp = tempfile::tempdir().unwrap();
        // SAFETY: ENV_LOCK serializes this test's isolated store environment.
        unsafe { std::env::set_var("PINVOU3_HOME", tmp.path()) };
        let store = SessionAgentStore::load().unwrap();
        let busy = AtomicBool::new(false);
        let configuring = Arc::new(AtomicBool::new(false));
        let guard = Arc::new(begin_configuration(&busy, configuring.clone(), "busy").unwrap());
        let (started_tx, started_rx) = tokio::sync::oneshot::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let (finished_tx, finished_rx) = tokio::sync::oneshot::channel();
        let old_store = store.clone();
        let caller = tokio::spawn(persist_with_configuration(guard, move || {
            started_tx.send(()).unwrap();
            release_rx
                .recv_timeout(std::time::Duration::from_secs(5))
                .unwrap();
            old_store
                .set_acp_config_value("session", "model", "old")
                .unwrap();
            finished_tx.send(()).unwrap();
        }));
        tokio::time::timeout(std::time::Duration::from_secs(5), started_rx)
            .await
            .unwrap()
            .unwrap();
        caller.abort();
        assert!(caller.await.unwrap_err().is_cancelled());
        assert!(configuring.load(Ordering::Acquire));
        assert!(begin_configuration(&busy, configuring.clone(), "busy").is_err());
        assert!(begin_prompt(&busy, &configuring).is_err());
        release_tx.send(()).unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(5), finished_rx)
            .await
            .unwrap()
            .unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(5), async {
            while configuring.load(Ordering::Acquire) {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        let next = Arc::new(begin_configuration(&busy, configuring.clone(), "busy").unwrap());
        persist_with_configuration(next, move || {
            store
                .set_acp_config_value("session", "model", "new")
                .unwrap();
        })
        .await
        .unwrap();
        assert!(!configuring.load(Ordering::Acquire));
        assert_eq!(
            SessionAgentStore::load()
                .unwrap()
                .get("session")
                .acp_config_values["model"],
            "new"
        );
    }

    #[test]
    fn remote_configuration_retirement_is_not_a_mode_capability_failure() {
        let runtime = super::super::lifecycle::RuntimeRetirementGate::default();
        let busy = AtomicBool::new(false);
        let configuring = AtomicBool::new(false);
        let begin = || begin_configuration(&busy, &configuring, "task_session_busy");
        let guard = finish_remote_configuration_admission(runtime.with_active(begin)).unwrap();
        assert!(configuring.load(Ordering::Acquire));
        drop(guard);
        assert!(!configuring.load(Ordering::Acquire));

        busy.store(true, Ordering::Release);
        let busy_error = finish_remote_configuration_admission(runtime.with_active(begin))
            .err()
            .unwrap();
        assert_eq!(busy_error.to_string(), "task_session_busy");
        assert!(!configuring.load(Ordering::Acquire));
        busy.store(false, Ordering::Release);
        assert!(runtime.close_if(|| true));
        let retired = finish_remote_configuration_admission(runtime.with_active(begin))
            .err()
            .unwrap();
        assert_eq!(retired.to_string(), "remote_turn_runtime_retired");
        assert!(!configuring.load(Ordering::Acquire));
        for error in [busy_error, retired] {
            assert_eq!(
                crate::features::assistant::remote_turn::remote_engine_error_code(&error),
                "task_engine_unavailable"
            );
            assert!(
                !error
                    .chain()
                    .any(|cause| cause.to_string() == "task_permission_mode_unsupported")
            );
        }
    }

    #[test]
    fn remote_mode_ack_cannot_keep_inherited_full_access_or_omit_capability() {
        let options = |current| {
            vec![serde_json::from_value::<super::super::SessionConfigOption>(json!({
            "id":"mode", "name":"Mode", "type":"select", "currentValue":current,
            "options":[{"value":"read-only","name":"Read-only"},{"value":"agent","name":"Agent"},{"value":"agent-full-access","name":"Full access"}]
        })).unwrap()]
        };
        assert!(verify_remote_mode_ack(&options("read-only"), "read-only").is_ok());
        assert!(verify_remote_mode_ack(&options("agent"), "agent").is_ok());
        assert!(verify_remote_mode_ack(&options("agent-full-access"), "agent").is_err());
        assert!(verify_remote_mode_ack(&options("agent"), "read-only").is_err());
        assert!(verify_remote_mode_ack(&[], "agent").is_err());
    }

    #[test]
    fn prompt_and_configuration_slots_are_mutually_exclusive() {
        let busy = AtomicBool::new(false);
        let configuring = AtomicBool::new(false);

        let guard = begin_configuration(&busy, &configuring, "busy").unwrap();
        assert!(begin_prompt(&busy, &configuring).is_err());
        drop(guard);

        begin_prompt(&busy, &configuring).unwrap();
        assert!(begin_configuration(&busy, &configuring, "busy").is_err());
    }

    #[test]
    fn configuration_guard_releases_on_drop() {
        let busy = AtomicBool::new(false);
        let configuring = AtomicBool::new(false);
        {
            let _guard = begin_configuration(&busy, &configuring, "busy").unwrap();
            assert!(configuring.load(Ordering::Acquire));
        }
        assert!(!configuring.load(Ordering::Acquire));
    }

    /// The timing turn must be registered inside the admitted section, before
    /// the caller can race an immediate completion. Simulates the production
    /// sequence in `AcpPool::send_message`: `admit_prompt_turn` (admission +
    /// activity touch + registration), then a synchronous completion arriving
    /// before the method would have returned. The terminal must be attributed
    /// to the registered turn and the queue must be empty afterwards; a
    /// caller-side registration after the await would instead leave the
    /// completion dropped and a stale unpaired entry behind.
    #[test]
    fn admitted_prompt_turn_survives_immediate_completion() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let tmp = std::env::temp_dir().join(format!(
            "pinvou3-acp-admission-timing-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        // SAFETY: the caller's test holds platform::paths::tests::ENV_LOCK throughout; env writes are serialized in-process.
        unsafe { std::env::set_var("PINVOU3_HOME", &tmp) };

        let busy = AtomicBool::new(false);
        let configuring = AtomicBool::new(false);
        let sid = "acp-admission-immediate-completion";

        admit_prompt_turn(&busy, &configuring, sid, || Ok(())).unwrap();
        // Admission registered the turn synchronously, before any spawned
        // prompt task can run: the queue holds it at this point.
        assert!(crate::features::assistant::timing::has_queued_active_turn(
            sid
        ));
        // Immediate completion: happens-before any post-await caller code.
        // admit_prompt_turn has already registered the turn, so the terminal
        // lands on it instead of being dropped on an empty queue.
        crate::features::assistant::timing::finish_turn(sid, "Completed", None);

        let timeline = crate::features::assistant::timing::read_timeline(sid).unwrap();
        let done: Vec<_> = timeline
            .iter()
            .filter(|e| e.event == "assistant_done")
            .collect();
        assert_eq!(done.len(), 1, "immediate completion must be recorded");
        assert_eq!(
            timeline[0].event, "user_start",
            "terminal must pair with the registered turn"
        );
        assert_eq!(done[0].turn_id, timeline[0].turn_id);
        assert!(
            !crate::features::assistant::timing::has_queued_active_turn(sid),
            "queue must be empty after the paired finish"
        );

        let _ = std::fs::remove_dir_all(tmp);
    }

    /// A submit rejected by busy admission must neither enqueue a ghost turn
    /// nor disturb the in-flight turn's queue entry: its send_error finish
    /// (if the caller emits one) finds only the in-flight turn and would be
    /// attributed correctly, and the in-flight terminal is never swallowed.
    #[test]
    fn rejected_concurrent_submit_never_clears_in_flight_turn() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let tmp = std::env::temp_dir().join(format!(
            "pinvou3-acp-reject-timing-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        // SAFETY: the caller's test holds platform::paths::tests::ENV_LOCK throughout; env writes are serialized in-process.
        unsafe { std::env::set_var("PINVOU3_HOME", &tmp) };

        let busy = AtomicBool::new(false);
        let configuring = AtomicBool::new(false);
        let sid = "acp-admission-rejected-submit";

        // First submit is admitted and its turn registered.
        admit_prompt_turn(&busy, &configuring, sid, || Ok(())).unwrap();
        // Concurrent second submit is rejected by busy admission: no turn is
        // registered (no ghost queue entry).
        let rejected = admit_prompt_turn(&busy, &configuring, sid, || Ok(()));
        assert!(rejected.is_err());
        assert!(
            !crate::features::assistant::timing::has_extra_queued_turns(sid),
            "rejected submit must not enqueue a second queue entry"
        );

        // The in-flight turn finishes; a late send_error from the rejected
        // caller afterwards must be a no-op (empty queue), so the in-flight
        // terminal is recorded exactly once and never swallowed.
        crate::features::assistant::timing::finish_turn(sid, "Completed", None);
        crate::features::assistant::timing::finish_turn(sid, "send_error", Some("busy"));

        let timeline = crate::features::assistant::timing::read_timeline(sid).unwrap();
        let done: Vec<_> = timeline
            .iter()
            .filter(|e| e.event == "assistant_done")
            .collect();
        assert_eq!(
            done.len(),
            1,
            "in-flight terminal must be recorded exactly once"
        );
        assert_eq!(done[0].status.as_deref(), Some("Completed"));

        let _ = std::fs::remove_dir_all(tmp);
    }

    /// Round-8 regression (failure injection): an activity-touch failure after
    /// successful busy admission must leave no residue of any kind — the busy
    /// slot is released for the next submit, no timing queue entry remains,
    /// and no `user_start` lifecycle event is persisted (registration writes
    /// both, and with no prompt task spawned nothing would ever pair a
    /// terminal with them). The next admitted submit must then behave as if
    /// the failed one never happened.
    #[test]
    fn activity_touch_failure_releases_slot_and_defers_registration() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let tmp = std::env::temp_dir().join(format!(
            "pinvou3-acp-touch-failure-timing-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        // SAFETY: the caller's test holds platform::paths::tests::ENV_LOCK throughout; env writes are serialized in-process.
        unsafe { std::env::set_var("PINVOU3_HOME", &tmp) };

        let busy = AtomicBool::new(false);
        let configuring = AtomicBool::new(false);
        let sid = "acp-admission-touch-failure";

        // Injected store failure: the session store cannot persist the
        // activity update (disk full, corrupted session file, invalid id...).
        let failed = admit_prompt_turn(&busy, &configuring, sid, || {
            Err(anyhow::anyhow!("simulated touch_activity failure"))
        });
        assert!(failed.is_err(), "touch failure must propagate to caller");

        // Busy slot rolled back: the session is immediately submittable.
        assert!(
            !busy.load(Ordering::Acquire),
            "busy slot must be released on touch failure"
        );
        // No queue entry was registered for the failed submit.
        assert!(
            !crate::features::assistant::timing::has_queued_active_turn(sid),
            "failed submit must not leave a queued timing entry"
        );
        // No lifecycle event was persisted: there is no accepted turn whose
        // start a later terminal could be misattributed to.
        let timeline = crate::features::assistant::timing::read_timeline(sid).unwrap();
        assert!(
            timeline.is_empty(),
            "failed submit must not persist any timing event"
        );

        // The next submit on the same session is unaffected: admitted, turn
        // registered, and a terminal pairs with it exactly.
        admit_prompt_turn(&busy, &configuring, sid, || Ok(())).unwrap();
        assert!(crate::features::assistant::timing::has_queued_active_turn(
            sid
        ));
        crate::features::assistant::timing::finish_turn(sid, "Completed", None);

        let timeline = crate::features::assistant::timing::read_timeline(sid).unwrap();
        let done: Vec<_> = timeline
            .iter()
            .filter(|e| e.event == "assistant_done")
            .collect();
        assert_eq!(done.len(), 1, "next turn terminal recorded exactly once");
        assert_eq!(timeline[0].event, "user_start");
        assert_eq!(done[0].turn_id, timeline[0].turn_id);

        let _ = std::fs::remove_dir_all(tmp);
    }
}
