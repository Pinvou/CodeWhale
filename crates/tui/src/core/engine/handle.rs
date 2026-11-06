//! Public `EngineHandle` methods.
//!
//! The struct itself lives next door in `engine.rs` because two
//! construction sites (`Engine::new` and the test-only
//! `mock_engine_handle`) need access to its private mpsc channels.
//! The method surface — `send`, `cancel*`, `is_cancelled`,
//! `approve_tool_call` / `deny_tool_call` / `retry_tool_with_policy`,
//! `submit_user_input` / `cancel_user_input`, and `steer` — moves here
//! so the agent loop's mailbox API is reviewable on its own.

use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Duration;

use anyhow::{Result, anyhow};
use tokio::sync::{mpsc, oneshot};

use super::approval::{ApprovalDecision, UserInputDecision};
use super::{
    CancelMode, CancelReason, EngineHandle, EngineRunCompletion, LiveRuntimeAuthority, Op,
    ReservedSteer, RuntimePermissionAuthority, SteerWithdrawal, UserInputResponse,
};

/// Bound on waiting for the engine to acknowledge an approval decision.
/// A live engine answers quickly; this only backstops a wedged engine.
pub(super) const APPROVAL_ACK_TIMEOUT: Duration = Duration::from_secs(15);

/// Extra settle window after the ack deadline. The engine sends the ack
/// only after its durable receipt commit finished and that commit has no
/// deadline, so the handle keeps waiting on the same oneshot instead of
/// reporting a false "not applied" while persistence is merely slow.
pub(super) const APPROVAL_ACK_SETTLE_TIMEOUT: Duration = Duration::from_secs(75);

/// Terminal result when neither the ack deadline nor the settle window
/// produced an engine answer. Deliberately distinct from "not applied":
/// the engine answers only after the durable receipt commit finishes and
/// that commit has no deadline, so the decision may still be committed
/// and the tool executed after this error is returned. Callers must
/// surface the outcome as unconfirmed instead of claiming the decision
/// was refused.
#[derive(Debug)]
pub(crate) struct ApprovalAckUnknownError {
    ack_timeout: Duration,
    settle_timeout: Duration,
}

impl ApprovalAckUnknownError {
    /// Test-only constructor for consumers pinning the remote mapping.
    #[cfg(test)]
    pub(crate) fn for_test(ack_timeout: Duration, settle_timeout: Duration) -> Self {
        Self {
            ack_timeout,
            settle_timeout,
        }
    }
}

impl std::fmt::Display for ApprovalAckUnknownError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "approval decision outcome is unknown: the engine did not \
             acknowledge within {}s ack + {}s settle, and the decision may \
             still be committed and executed",
            self.ack_timeout.as_secs(),
            self.settle_timeout.as_secs()
        )
    }
}

impl std::error::Error for ApprovalAckUnknownError {}

/// One bounded wait on the approval ack oneshot. `TimedOut` deliberately
/// leaves the receiver alive with its caller: the decision may still be
/// mid-commit, and dropping the receiver here is what used to turn a slow
/// commit into a false "not applied".
enum AckAnswer {
    Applied,
    NotApplied(anyhow::Error),
    TimedOut,
}

async fn ack_answer(rx_ack: &mut oneshot::Receiver<bool>, budget: Duration) -> AckAnswer {
    match tokio::time::timeout(budget, &mut *rx_ack).await {
        Ok(Ok(true)) => AckAnswer::Applied,
        Ok(Ok(false)) => AckAnswer::NotApplied(anyhow!(
            "approval decision was not applied: the tool call id is stale \
             or its outcome could not be committed"
        )),
        Ok(Err(_)) => AckAnswer::NotApplied(anyhow!(
            "approval decision was dropped without being consumed by the engine"
        )),
        Err(_elapsed) => AckAnswer::TimedOut,
    }
}

impl EngineHandle {
    /// Retire only at the actor's own idle boundary, preserving active turns,
    /// queued inputs, child/shell results and goal continuations by refusing.
    /// `false` also covers states whose safe retirement cannot be certified,
    /// including an outstanding advisor watcher or an event still sitting in
    /// the host's queue: the host must keep consuming events until the
    /// handshake completes. It does not
    /// promise that waiting alone will eventually make retirement available.
    ///
    /// The call blocks until the actor reaches its next input boundary, so
    /// retry loops must back off instead of spinning. The embedding host must
    /// exclude new external work until retirement is complete — including
    /// work still held by an in-flight `reserve()` permit, which the actor
    /// cannot observe. `host_is_idle` checks its own consumers synchronously
    /// while the actor is sealed: dequeuing an event does not prove host
    /// persistence or terminal delivery finished.
    ///
    /// Refusing resumes the actor without shutdown; so does a cancel token
    /// firing for the first time during an uncommitted wait (a token already
    /// latched by an earlier cancelled turn does not refuse). `Err` means no
    /// proposal is answerable — the request channel or a oneshot counterpart
    /// was dropped — not that the engine is unhealthy; the cancel-beats-commit
    /// race also surfaces as `Err("idle-retirement proposal expired")`, so
    /// treat every `Err` as "not retired" and retry. `true` means the actor
    /// consumed the commit — the send receipt alone is not proof, since a
    /// send racing a simultaneous cancel can succeed on a proposal the actor
    /// abandons; a lost commit ack therefore reports `false`. `true` means
    /// committed, not completed; wait_run_completion is still required before
    /// touching resources. Cancelling an uncommitted wait cannot cause a late
    /// shutdown.
    pub async fn shutdown_when_idle(&self, host_is_idle: impl FnOnce() -> bool) -> Result<bool> {
        let (ready, receipt) = oneshot::channel();
        let (commit, committed) = oneshot::channel();
        let (commit_ack, ack) = oneshot::channel();
        self.idle_retirement
            .as_ref()
            .ok_or_else(|| anyhow!("engine idle-retirement is unavailable"))?
            .send(super::idle_shutdown::IdleRetirement {
                ready,
                commit: committed,
                commit_ack,
            })
            .await?;
        if !receipt
            .await
            .map_err(|_| anyhow!("engine idle-retirement request dropped"))?
        {
            return Ok(false);
        }
        // Keep both owners at the same boundary. There is no await between this
        // host check and commit. A refused host or cancelled proposal drops the
        // commit sender, so the actor resumes instead of leaving a closed handle.
        if !host_is_idle() {
            return Ok(false);
        }
        commit
            .send(())
            .map_err(|_| anyhow!("engine idle-retirement proposal expired"))?;
        // Only the actor's ack proves it consumed the commit; the send above
        // can also succeed against a proposal the cancel arm just abandoned.
        if ack.await.is_err() {
            return Ok(false);
        }
        Ok(true)
    }

    /// Wait for this exact background run to return or terminate abnormally.
    /// Cancelling a wait does not stop the engine or consume another waiter's result.
    /// Directly constructed handles return `NotObserved`; completion does not prove
    /// persistence succeeded or detached children and blocking writers stopped.
    /// The wait owns only its receipt and may outlive the handle without keeping
    /// the engine's operation sender or event queue alive.
    pub fn wait_run_completion(
        &self,
    ) -> impl std::future::Future<Output = EngineRunCompletion> + Send + 'static + use<> {
        let completion = self.run_completion.clone();
        async move {
            let Some(mut completion) = completion else {
                return EngineRunCompletion::NotObserved;
            };
            loop {
                let outcome = *completion.borrow_and_update();
                if let Some(outcome) = outcome {
                    return outcome;
                }
                if completion.changed().await.is_err() {
                    return EngineRunCompletion::AbnormalTermination;
                }
            }
        }
    }

    /// True when the caller must preflight a concrete provider client before
    /// committing UI/runtime turn state. Test and embedding handles with an
    /// injected model client return false because that client owns model I/O.
    #[must_use]
    pub(crate) fn client_preflight_required(&self) -> bool {
        self.client_preflight_required
    }

    /// Send an operation to the engine
    pub async fn send(&self, op: Op) -> Result<()> {
        let authority = Self::change_mode_authority(&op);
        let permit = self.tx_op.reserve().await?;
        if let Some(authority) = authority {
            self.publish_runtime_authority(authority);
        }
        permit.send(op);
        Ok(())
    }

    /// Try to send an operation without blocking.
    ///
    /// Returns `Err` if the channel is full or closed.  Use this for
    /// non-critical, refresh-type ops (e.g. `Op::ListSubAgents`) that can
    /// safely be dropped and re-requested on the next drain cycle.
    pub fn try_send(&self, op: Op) -> Result<()> {
        let authority = Self::change_mode_authority(&op);
        let result = self.tx_op.try_send(op);
        // A full channel already guarantees that the engine will wake and
        // drain an operation. Publish the typed authority anyway: the drain
        // applies pending authority before handling that queued operation, so
        // a posture edit never blocks behind refresh traffic. A closed
        // channel has no engine left to observe the update.
        if !matches!(&result, Err(mpsc::error::TrySendError::Closed(_)))
            && let Some(authority) = authority
        {
            self.publish_runtime_authority(authority);
        }
        result?;
        Ok(())
    }

    fn change_mode_authority(op: &Op) -> Option<LiveRuntimeAuthority> {
        let Op::ChangeMode {
            mode,
            allow_shell,
            trust_mode,
            auto_approve,
            approval_mode,
            configured_sandbox_mode,
            ask_on_guardrail,
        } = op
        else {
            return None;
        };
        Some(LiveRuntimeAuthority::from_fields(
            *mode,
            *allow_shell,
            *trust_mode,
            *auto_approve,
            *approval_mode,
            configured_sandbox_mode.clone(),
            *ask_on_guardrail,
        ))
    }

    fn publish_runtime_authority(&self, authority: LiveRuntimeAuthority) {
        let mut state = self
            .live_runtime_authority
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        // Pinvou tri-mode safety: `None` arming means "keep the session's
        // current arming" — the same doctrine `apply_change_mode` applies —
        // so a posture-only publish (a turn install, or a ChangeMode op that
        // carries no arming) can never erase a host arming published ahead
        // of it in the queue. The op loop applies this snapshot, never the
        // queued op payload, so an erasing publish would silently drop the
        // arm request with no event and no receipt. Only an explicit `Some`
        // may move arming.
        let mut authority = authority;
        if authority.ask_on_guardrail.is_none() {
            authority.ask_on_guardrail = state.authority.ask_on_guardrail;
        }
        state.revision = state.revision.wrapping_add(1).max(1);
        state.authority = authority;
    }

    // Pinvou tri-mode safety added the ask-on-guardrail carrier; the
    // parameter set mirrors LiveRuntimeAuthority::from_fields.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn publish_turn_authority(
        &self,
        mode: crate::tui::app::AppMode,
        allow_shell: bool,
        trust_mode: bool,
        auto_approve: bool,
        approval_mode: crate::tui::approval::ApprovalMode,
        configured_sandbox_mode: Option<String>,
        ask_on_guardrail: Option<bool>,
    ) {
        self.publish_runtime_authority(LiveRuntimeAuthority::from_fields(
            mode,
            allow_shell,
            trust_mode,
            auto_approve,
            approval_mode,
            configured_sandbox_mode,
            ask_on_guardrail,
        ));
    }

    /// Exact live permission authority for runtime approval and elevation
    /// gates. This is the same typed state the active engine turn drains.
    #[must_use]
    pub(crate) fn runtime_permission_authority(&self) -> RuntimePermissionAuthority {
        self.live_runtime_authority
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .authority
            .permission_snapshot()
    }

    /// Reserve capacity for a runtime steer before it mutates durable state.
    /// The owned permit lets the caller persist and dispatch synchronously,
    /// without a cancellation point between those two operations.
    pub(crate) async fn reserve_steer(&self) -> Result<ReservedSteer> {
        let id = self.next_steer_id();
        let target = self
            .steer_control
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .active_target()
            .map_err(anyhow::Error::msg)?;
        let permit = self.tx_steer.clone().reserve_owned().await?;
        self.steer_control
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .register(id.clone(), target)
            .map_err(anyhow::Error::msg)?;
        Ok(ReservedSteer {
            permit: Some(permit),
            sent: false,
            id,
            target,
            control: Arc::clone(&self.steer_control),
        })
    }

    fn next_steer_id(&self) -> String {
        let seq = self.next_steer_id.fetch_add(1, Ordering::Relaxed) + 1;
        format!("steer-{seq}")
    }

    /// Cancel the current request (user-initiated path — keeps the
    /// public `cancel()` signature stable). Equivalent to
    /// `cancel_with_reason(CancelReason::User)`.
    pub fn cancel(&self) {
        self.cancel_with_mode(CancelReason::User, CancelMode::StopDropInbox);
    }

    /// Cancel the current request and latch the reason so downstream
    /// "request cancelled" error messages can name a cause.
    pub fn cancel_with_reason(&self, reason: CancelReason) {
        self.cancel_with_mode(reason, CancelMode::StopDropInbox);
    }

    /// Atomically publish the steer disposition and cancel the active turn.
    pub fn cancel_with_mode(&self, reason: CancelReason, mode: CancelMode) {
        self.publish_cancel_disposition(reason, mode);
        match self.cancel_token.lock() {
            Ok(slot) => slot.token.cancel(),
            Err(poisoned) => poisoned.into_inner().token.cancel(),
        }
        crate::retry_status::clear();
    }

    /// Cancel exactly the turn named by `turn_id`, and only while that turn
    /// still holds the shared cancellation slot.
    ///
    /// Returns `true` when the slot named `turn_id` and its token fired;
    /// `false` when the slot already moved on — the engine swapped tokens for
    /// a newer turn (a host-driven send, or a runtime self-start such as an
    /// idle sub-agent completion, a background shell wake, or a goal
    /// continuation) before this cancel was observed. In the `false` case
    /// nothing is cancelled and no steer disposition or cancel reason is
    /// published: the target turn is already gone, and the newer turn belongs
    /// to a different generation the caller never saw.
    ///
    /// The identity check, the steer disposition, and the token resolution
    /// all happen under the same slot lock the engine's turn-start install
    /// uses, so a concurrent turn start can neither race the decision nor
    /// inherit this cancel's disposition: the install waits for the lock, and
    /// by the time it swaps the slot the disposition has already landed on
    /// the named turn. The token is cloned out while locked, and firing it
    /// afterwards cancels only that cloned token — even if the slot moves on
    /// in between, the fired object is still the target turn's own token.
    #[must_use = "a false return means the target turn already ended; do not treat the cancel as delivered"]
    pub fn cancel_turn(&self, turn_id: &str, reason: CancelReason, mode: CancelMode) -> bool {
        let token = {
            let slot = self
                .cancel_token
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if slot.turn_id.as_deref() != Some(turn_id) {
                return false;
            }
            // Publish while the slot lock is still held: a turn start that
            // swaps the slot waits for this lock, so the disposition can
            // never latch onto a follow-up turn the caller never targeted.
            self.publish_cancel_disposition(reason, mode);
            slot.token.clone()
        };
        token.cancel();
        crate::retry_status::clear();
        true
    }

    /// Publish a stop's steer disposition without firing any cancellation
    /// token. For hosts whose target turn already completed (the app's
    /// terminal-closing window): parked steers must still be dropped and the
    /// cancel reason latched, but the shared slot may already hold a runtime
    /// self-started follow-up turn's live token (pinvou-agent#254), so no
    /// token may fire here.
    pub fn publish_stop_disposition(&self, reason: CancelReason, mode: CancelMode) {
        self.publish_cancel_disposition(reason, mode);
        crate::retry_status::clear();
    }

    /// Publish the steer disposition and latch the cancel reason shared by
    /// every cancel entry point. The token fire itself stays with the caller
    /// so turn-bound cancels can resolve the exact token under the slot lock.
    fn publish_cancel_disposition(&self, reason: CancelReason, mode: CancelMode) {
        self.steer_control
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .cancel(mode);
        match self.cancel_reason.lock() {
            Ok(mut slot) => *slot = Some(reason),
            Err(poisoned) => *poisoned.into_inner() = Some(reason),
        }
    }

    /// Check if a request is currently cancelled
    #[must_use]
    #[allow(dead_code)]
    pub fn is_cancelled(&self) -> bool {
        match self.cancel_token.lock() {
            Ok(slot) => slot.token.is_cancelled(),
            Err(poisoned) => poisoned.into_inner().token.is_cancelled(),
        }
    }

    /// Pause or resume the current pausable command.
    pub fn set_paused(&self, paused: bool) {
        match self.shared_paused.lock() {
            Ok(mut slot) => *slot = paused,
            Err(poisoned) => *poisoned.into_inner() = paused,
        }
    }

    /// Check whether the engine pause gate is set.
    #[cfg(test)]
    #[must_use]
    pub fn is_paused(&self) -> bool {
        match self.shared_paused.lock() {
            Ok(slot) => *slot,
            Err(poisoned) => *poisoned.into_inner(),
        }
    }

    /// Approve a pending tool call.
    ///
    /// `Ok(())` means the engine consumed the decision and durably committed
    /// the approval for this exact id (or routed it to a waiting child).
    /// `Err` means exactly one of:
    ///
    /// - the decision was refused (stale id, its durable outcome could not
    ///   be committed, or the engine dropped it unconsumed): the tool is
    ///   not approved;
    /// - the outcome is unknown ([`ApprovalAckUnknownError`]): neither the
    ///   ack deadline nor the settle window got an answer. The engine's
    ///   durable commit has no deadline, so the decision may still be
    ///   committed and executed later; the caller must report the outcome
    ///   as unconfirmed — not as "not approved".
    pub async fn approve_tool_call(&self, id: impl Into<String>) -> Result<()> {
        let (ack, rx_ack) = oneshot::channel();
        self.tx_approval
            .send(ApprovalDecision::Approved {
                id: id.into(),
                ack: Some(ack),
            })
            .await?;
        Self::await_approval_ack(
            rx_ack,
            self.approval_ack_timeout,
            self.approval_ack_settle_timeout,
        )
        .await
    }

    /// Deny a pending tool call. The ack contract is the same as
    /// [`EngineHandle::approve_tool_call`].
    pub async fn deny_tool_call(&self, id: impl Into<String>) -> Result<()> {
        let (ack, rx_ack) = oneshot::channel();
        self.tx_approval
            .send(ApprovalDecision::Denied {
                id: id.into(),
                ack: Some(ack),
            })
            .await?;
        Self::await_approval_ack(
            rx_ack,
            self.approval_ack_timeout,
            self.approval_ack_settle_timeout,
        )
        .await
    }

    /// Retry a tool call with an elevated sandbox policy. The ack contract
    /// is the same as [`EngineHandle::approve_tool_call`].
    pub async fn retry_tool_with_policy(
        &self,
        id: impl Into<String>,
        policy: crate::sandbox::SandboxPolicy,
    ) -> Result<()> {
        let (ack, rx_ack) = oneshot::channel();
        self.tx_approval
            .send(ApprovalDecision::RetryWithPolicy {
                id: id.into(),
                policy,
                ack: Some(ack),
            })
            .await?;
        Self::await_approval_ack(
            rx_ack,
            self.approval_ack_timeout,
            self.approval_ack_settle_timeout,
        )
        .await
    }

    /// Wait for the engine to acknowledge a delivered decision, then keep
    /// converging through a settle window.
    ///
    /// The engine sends the ack only after the durable receipt commit
    /// finished, so an answer received in either window is always the real
    /// terminal outcome. The fast ack deadline backstops a wedged engine;
    /// the settle window then covers the slow-persistence case (the receipt
    /// append waits on a file lock and fsync with no deadline): the receiver
    /// is deliberately kept alive so a decision whose commit merely crossed
    /// the deadline resolves to its true outcome instead of a false "not
    /// applied". Only when both windows expire does the caller receive
    /// [`ApprovalAckUnknownError`].
    async fn await_approval_ack(
        rx_ack: oneshot::Receiver<bool>,
        ack_timeout: Duration,
        settle_timeout: Duration,
    ) -> Result<()> {
        let mut rx_ack = rx_ack;
        match ack_answer(&mut rx_ack, ack_timeout).await {
            AckAnswer::Applied => Ok(()),
            AckAnswer::NotApplied(error) => Err(error),
            AckAnswer::TimedOut => match ack_answer(&mut rx_ack, settle_timeout).await {
                AckAnswer::Applied => Ok(()),
                AckAnswer::NotApplied(error) => Err(error),
                AckAnswer::TimedOut => Err(anyhow::Error::new(ApprovalAckUnknownError {
                    ack_timeout,
                    settle_timeout,
                })),
            },
        }
    }

    /// Override the ack/settle windows in tests so slow-persistence
    /// regressions run in milliseconds instead of wall-clock minutes.
    #[cfg(test)]
    pub(crate) fn set_approval_ack_windows_for_test(&mut self, ack: Duration, settle: Duration) {
        self.approval_ack_timeout = ack;
        self.approval_ack_settle_timeout = settle;
    }

    /// Submit a response for request_user_input.
    pub async fn submit_user_input(
        &self,
        id: impl Into<String>,
        response: UserInputResponse,
    ) -> Result<()> {
        self.tx_user_input
            .send(UserInputDecision::Submitted {
                id: id.into(),
                response,
            })
            .await?;
        Ok(())
    }

    /// Cancel a request_user_input prompt.
    pub async fn cancel_user_input(&self, id: impl Into<String>) -> Result<()> {
        self.tx_user_input
            .send(UserInputDecision::Cancelled { id: id.into() })
            .await?;
        Ok(())
    }

    /// Steer an in-flight turn with additional user input.
    pub fn withdraw_steer(&self, steer_id: &str) -> SteerWithdrawal {
        self.steer_control
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .withdraw(steer_id)
    }

    /// Steer an in-flight turn and return its opaque correlation id.
    pub async fn steer(&self, content: impl Into<String>) -> Result<String> {
        Ok(self.reserve_steer().await?.send(content.into()))
    }

    /// Request a snapshot of the current session state.
    /// Returns the snapshot directly via a oneshot channel, avoiding
    /// competition with the SSE event stream on the mpsc receiver.
    pub async fn get_session_snapshot(&self) -> Result<crate::core::ops::SessionSnapshot> {
        let (tx, rx) = tokio::sync::oneshot::channel();
        let tx = std::sync::Arc::new(std::sync::Mutex::new(Some(tx)));
        self.send(Op::GetSessionSnapshot { tx }).await?;
        rx.await
            .map_err(|_| anyhow::anyhow!("Engine dropped session snapshot oneshot"))
    }

    /// Request active provider request concurrency state.
    pub async fn get_provider_runtime_status(
        &self,
    ) -> Result<crate::core::ops::ProviderRuntimeStatus> {
        let (tx, rx) = tokio::sync::oneshot::channel();
        let tx = std::sync::Arc::new(std::sync::Mutex::new(Some(tx)));
        self.send(Op::GetProviderRuntimeStatus { tx }).await?;
        rx.await
            .map_err(|_| anyhow::anyhow!("Engine dropped provider runtime status oneshot"))
    }

    /// Run the bounded initial connection pass on the engine-owned MCP pool.
    ///
    /// The returned manager snapshot and every later tool call therefore see
    /// the same connections and catalog generation. Unlike `reload_mcp`, this
    /// does not force a config re-read or drop ready transports. Optional
    /// servers are connected in the background at engine spawn; this waits
    /// only if the caller explicitly asked for the settled receipt.
    pub async fn bootstrap_mcp(&self) -> Result<crate::core::ops::McpManagerUpdate> {
        let (tx, rx) = tokio::sync::oneshot::channel();
        let tx = std::sync::Arc::new(std::sync::Mutex::new(Some(tx)));
        self.send(Op::BootstrapMcp { tx }).await?;
        rx.await
            .map_err(|_| anyhow::anyhow!("Engine dropped MCP bootstrap oneshot"))?
            .map_err(anyhow::Error::msg)
    }

    /// Retry one failed server through the existing engine-owned pool.
    pub async fn retry_mcp_server(
        &self,
        name: impl Into<String>,
    ) -> Result<crate::core::ops::McpManagerUpdate> {
        let (tx, rx) = tokio::sync::oneshot::channel();
        let tx = std::sync::Arc::new(std::sync::Mutex::new(Some(tx)));
        self.send(Op::RetryMcpServer {
            name: name.into(),
            tx,
        })
        .await?;
        rx.await
            .map_err(|_| anyhow::anyhow!("Engine dropped MCP retry oneshot"))?
            .map_err(anyhow::Error::msg)
    }

    /// Force the engine-owned MCP pool to reload and reconnect, returning a
    /// snapshot from the exact live pool that supplies the next model turn.
    pub async fn reload_mcp(
        &self,
        config_path: std::path::PathBuf,
    ) -> Result<crate::core::ops::McpManagerUpdate> {
        let (tx, rx) = tokio::sync::oneshot::channel();
        let tx = std::sync::Arc::new(std::sync::Mutex::new(Some(tx)));
        self.send(Op::ReloadMcp { config_path, tx }).await?;
        rx.await
            .map_err(|_| anyhow::anyhow!("Engine dropped MCP reload oneshot"))?
            .map_err(anyhow::Error::msg)
    }
}
