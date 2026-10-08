use parking_lot::ReentrantMutex;
use std::cell::Cell;
use std::collections::{HashMap, VecDeque};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

/// Instance-local admission/publication fence. Reentrant because handling an
/// agent event can synchronously publish a derived event on the same bridge.
/// Never hold this synchronous fence across an await or a pending-map lock.
#[derive(Default)]
pub(super) struct RuntimeRetirementGate {
    closed: ReentrantMutex<Cell<bool>>,
}

impl RuntimeRetirementGate {
    pub(super) fn with_active<T>(&self, action: impl FnOnce() -> T) -> Option<T> {
        let closed = self.closed.lock();
        if closed.get() { None } else { Some(action()) }
    }

    pub(super) fn close_if(&self, claim: impl FnOnce() -> bool) -> bool {
        let closed = self.closed.lock();
        if closed.get() || !claim() {
            return false;
        }
        closed.set(true);
        true
    }

    pub(super) fn settle_turn(
        &self,
        busy: &AtomicBool,
        claim: impl FnOnce() -> bool,
        close_timing: impl FnOnce(),
    ) -> bool {
        self.with_active(|| finalize_claimed_turn(busy, claim(), close_timing))
            .unwrap_or(false)
    }
}

/// Only the terminal claimant closes timing, then releases admission.
pub(super) fn finalize_claimed_turn(
    busy: &AtomicBool,
    claimed: bool,
    close_timing: impl FnOnce(),
) -> bool {
    if !claimed {
        return false;
    }
    close_timing();
    busy.store(false, Ordering::Release);
    true
}

/// A delayed callback must not remove a newer card which reused the same key.
pub(super) fn remove_owned_callback<T>(
    pending: &mut HashMap<String, T>,
    key: &str,
    owns: impl FnOnce(&T) -> bool,
) -> Option<T> {
    if pending.get(key).is_some_and(owns) {
        pending.remove(key)
    } else {
        None
    }
}

/// Caller holds the pool registry lock. Finalize a captured resident before
/// removing its slot; a replacement must never seed from an unfinished tail.
pub(super) fn take_finalized_runtime<T>(
    sessions: &mut HashMap<String, T>,
    session_id: &str,
    finalize: impl FnOnce(&T),
) -> Option<T> {
    finalize(sessions.get(session_id)?);
    sessions.remove(session_id)
}

pub(super) fn finish_owned_callback<T>(
    pending: &mut HashMap<String, T>,
    key: &str,
    owns: impl FnOnce(&T) -> bool,
    resolved: impl FnOnce(),
) -> bool {
    if remove_owned_callback(pending, key, owns).is_none() {
        return false;
    }
    resolved();
    true
}

/// A delayed handler may remove only its captured runtime, never a replacement.
pub(super) fn take_owned_runtime<T>(
    sessions: &mut HashMap<String, T>,
    session_id: &str,
    owns: impl FnOnce(&T) -> bool,
    finalize: impl FnOnce(&T),
) -> Option<T> {
    if !sessions.get(session_id).is_some_and(owns) {
        return None;
    }
    take_finalized_runtime(sessions, session_id, finalize)
}

const CRASH_WINDOW: Duration = Duration::from_secs(60);
const STABLE_RESET_AFTER: Duration = Duration::from_secs(2 * 60);
const MAX_CRASHES_IN_WINDOW: usize = 3;
const SECOND_RETRY_DELAY: Duration = Duration::from_millis(250);

/// Per-process lifetime state for one ACP runtime generation.
///
/// The state belongs to the runtime instance instead of the session id. A
/// delayed close from an old generation therefore cannot mark a replacement
/// generation dead.
pub(super) struct RuntimeLifecycle {
    generation: u64,
    started_at: Instant,
    running: AtomicBool,
    expected_shutdown: AtomicBool,
    unexpected_exit: AtomicBool,
    crash_counted: AtomicBool,
}

pub(super) enum RuntimeLookup<T> {
    Reusable(T),
    Dead(T),
    Missing,
}

/// Resolves one session entry under the pool lock. A dead entry is removed in
/// the same critical section, so no caller can reuse it after the decision.
pub(super) fn resolve_runtime<T: Clone>(
    sessions: &mut HashMap<String, T>,
    session_id: &str,
    is_running: impl FnOnce(&T) -> bool,
) -> RuntimeLookup<T> {
    let Some(runtime) = sessions.get(session_id).cloned() else {
        return RuntimeLookup::Missing;
    };
    if is_running(&runtime) {
        RuntimeLookup::Reusable(runtime)
    } else {
        sessions.remove(session_id);
        RuntimeLookup::Dead(runtime)
    }
}

impl RuntimeLifecycle {
    pub(super) fn new(generation: u64) -> Self {
        Self {
            generation,
            started_at: Instant::now(),
            running: AtomicBool::new(true),
            expected_shutdown: AtomicBool::new(false),
            unexpected_exit: AtomicBool::new(false),
            crash_counted: AtomicBool::new(false),
        }
    }

    pub(super) fn generation(&self) -> u64 {
        self.generation
    }

    pub(super) fn is_running(&self) -> bool {
        self.running.load(Ordering::Acquire)
    }

    pub(super) fn stable_for(&self) -> Duration {
        self.started_at.elapsed()
    }

    pub(super) fn begin_expected_shutdown(&self) {
        self.expected_shutdown.store(true, Ordering::Release);
    }

    /// Marks transport completion and returns whether it was unexpected.
    pub(super) fn mark_connection_closed(&self) -> bool {
        let unexpected = !self.expected_shutdown.load(Ordering::Acquire);
        if unexpected {
            self.unexpected_exit.store(true, Ordering::Release);
        }
        self.running.store(false, Ordering::Release);
        unexpected
    }

    /// Returns true once for an unexpected exit, so a dead entry observed by
    /// concurrent callers consumes exactly one crash-budget slot.
    pub(super) fn take_uncounted_crash(&self) -> bool {
        self.unexpected_exit.load(Ordering::Acquire)
            && self
                .crash_counted
                .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
                .is_ok()
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) enum SpawnAdmission {
    Allowed,
    RetryAfter {
        delay: Duration,
        budget_exhausted: bool,
    },
}

#[derive(Default)]
pub(super) struct RuntimeCrashBudget {
    crashes: VecDeque<Instant>,
}

impl RuntimeCrashBudget {
    pub(super) fn record_crash_at(&mut self, now: Instant) {
        self.prune(now);
        self.crashes.push_back(now);
    }

    pub(super) fn admission_at(&mut self, now: Instant) -> SpawnAdmission {
        self.prune(now);
        let Some(last) = self.crashes.back().copied() else {
            return SpawnAdmission::Allowed;
        };
        if self.crashes.len() >= MAX_CRASHES_IN_WINDOW {
            let oldest = self.crashes.front().copied().unwrap_or(last);
            return retry_until(now, oldest + CRASH_WINDOW, true);
        }
        if self.crashes.len() == 1 {
            return SpawnAdmission::Allowed;
        }
        let cooldown = SECOND_RETRY_DELAY;
        retry_until(now, last + cooldown, false)
    }

    fn prune(&mut self, now: Instant) {
        while self
            .crashes
            .front()
            .is_some_and(|crash| now.saturating_duration_since(*crash) >= CRASH_WINDOW)
        {
            self.crashes.pop_front();
        }
    }
}

pub(super) fn runtime_is_stable(lifecycle: &RuntimeLifecycle) -> bool {
    lifecycle.is_running() && lifecycle.stable_for() >= STABLE_RESET_AFTER
}

fn retry_until(now: Instant, deadline: Instant, budget_exhausted: bool) -> SpawnAdmission {
    match deadline.checked_duration_since(now) {
        Some(delay) if !delay.is_zero() => SpawnAdmission::RetryAfter {
            delay,
            budget_exhausted,
        },
        _ => SpawnAdmission::Allowed,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn retirement_waits_for_the_whole_claim_timing_and_busy_settlement() {
        use std::sync::{Arc, mpsc};
        let gate = Arc::new(RuntimeRetirementGate::default());
        let busy = Arc::new(AtomicBool::new(true));
        let timing = Arc::new(parking_lot::Mutex::new(Some("old")));
        let (claimed_tx, claimed_rx) = mpsc::channel();
        let (finish_tx, finish_rx) = mpsc::channel();
        let (closed_tx, closed_rx) = mpsc::channel();
        let settling = {
            let gate = gate.clone();
            let busy = busy.clone();
            let timing = timing.clone();
            std::thread::spawn(move || {
                gate.settle_turn(
                    &busy,
                    || {
                        // Pause AFTER the terminal claim but BEFORE shared timing.
                        claimed_tx.send(()).unwrap();
                        true
                    },
                    || {
                        finish_rx.recv_timeout(Duration::from_secs(2)).unwrap();
                        assert_eq!(timing.lock().take(), Some("old"));
                    },
                )
            })
        };
        claimed_rx.recv_timeout(Duration::from_secs(2)).unwrap();
        let retiring = {
            let gate = gate.clone();
            std::thread::spawn(move || {
                assert!(gate.close_if(|| true));
                closed_tx.send(()).unwrap();
            })
        };
        assert!(matches!(
            closed_rx.recv_timeout(Duration::from_millis(25)),
            Err(mpsc::RecvTimeoutError::Timeout)
        ));
        assert!(busy.load(Ordering::Acquire));
        finish_tx.send(()).unwrap();
        assert!(settling.join().unwrap());
        closed_rx.recv_timeout(Duration::from_secs(2)).unwrap();
        retiring.join().unwrap();
        timing.lock().replace("successor");
        assert!(!gate.settle_turn(
            &busy,
            || true,
            || {
                timing.lock().take();
            }
        ));
        assert_eq!(*timing.lock(), Some("successor"));
        assert!(!busy.load(Ordering::Acquire));
    }

    #[test]
    fn upgrade_claim_and_closure_share_the_admission_fence() {
        let gate = RuntimeRetirementGate::default();
        let active_turn = Cell::new(Some("old"));
        let busy = Cell::new(true);
        assert!(gate.close_if(|| {
            assert_eq!(active_turn.replace(None), Some("old"));
            busy.set(false);
            true
        }));
        assert_eq!(
            gate.with_active(|| {
                assert!(!busy.replace(true));
                active_turn.set(Some("successor"));
            }),
            None
        );
        assert_eq!(active_turn.get(), None);
        assert!(!busy.get());
    }

    #[test]
    fn late_upgrade_claim_cannot_seal_same_runtime_successor() {
        let gate = RuntimeRetirementGate::default();
        let current = Cell::new(Some("successor"));
        assert!(!gate.close_if(|| {
            if current.get() != Some("old") {
                return false;
            }
            current.set(None);
            true
        }));
        assert_eq!(gate.with_active(|| current.get()), Some(Some("successor")));
    }

    #[test]
    fn late_upgrade_handler_cannot_take_replacement_or_publish_global_error() {
        use std::sync::Arc;
        let old = Arc::new(());
        let new = Arc::new(());
        let mut sessions = HashMap::from([("session".to_owned(), new.clone())]);
        let global_error = Cell::new(false);
        assert!(
            take_owned_runtime(
                &mut sessions,
                "session",
                |entry| Arc::ptr_eq(entry, &old),
                |_| global_error.set(true)
            )
            .is_none()
        );
        assert!(!global_error.get());
        assert!(Arc::ptr_eq(sessions.get("session").unwrap(), &new));
        assert!(
            take_owned_runtime(
                &mut sessions,
                "session",
                |entry| Arc::ptr_eq(entry, &new),
                |_| global_error.set(true)
            )
            .is_some()
        );
        assert!(global_error.get());
    }

    #[test]
    fn retirement_fences_admission_events_and_answers_per_instance() {
        let old = RuntimeRetirementGate::default();
        let new = RuntimeRetirementGate::default();
        assert_eq!(old.with_active(|| old.with_active(|| 1)), Some(Some(1)));
        assert!(old.close_if(|| true));
        for _ in 0..3 {
            assert_eq!(
                old.with_active(|| panic!("retired side effect")),
                None::<()>
            );
        }
        assert!(!old.close_if(|| panic!("duplicate retirement")));
        assert_eq!(new.with_active(|| 2), Some(2));
    }

    #[test]
    fn stale_owned_turn_cannot_close_reused_runtime_gate() {
        let gate = RuntimeRetirementGate::default();
        let current_turn = std::cell::RefCell::new(Some("new"));
        assert!(!gate.close_if(|| {
            if *current_turn.borrow() != Some("old") {
                return false;
            }
            current_turn.replace(None);
            true
        }));
        assert_eq!(
            gate.with_active(|| *current_turn.borrow()),
            Some(Some("new"))
        );
    }

    #[test]
    fn retirement_waits_for_inflight_publication_then_rejects_late_publication() {
        use std::sync::{Arc, mpsc};
        let gate = Arc::new(RuntimeRetirementGate::default());
        let journal = Arc::new(parking_lot::Mutex::new(Vec::new()));
        let (entered_tx, entered_rx) = mpsc::channel();
        let (release_tx, release_rx) = mpsc::channel();
        let publishing = {
            let gate = gate.clone();
            let journal = journal.clone();
            std::thread::spawn(move || {
                gate.with_active(|| {
                    entered_tx.send(()).unwrap();
                    release_rx.recv_timeout(Duration::from_secs(2)).unwrap();
                    journal.lock().push("old accepted event");
                })
            })
        };
        entered_rx.recv_timeout(Duration::from_secs(2)).unwrap();
        let closing = {
            let gate = gate.clone();
            std::thread::spawn(move || gate.close_if(|| true))
        };
        release_tx.send(()).unwrap();
        assert_eq!(publishing.join().unwrap(), Some(()));
        assert!(closing.join().unwrap());
        assert_eq!(gate.with_active(|| journal.lock().push("late event")), None);
        assert_eq!(journal.lock().as_slice(), ["old accepted event"]);
    }

    #[test]
    fn old_callback_cannot_remove_new_card_with_same_key() {
        use std::sync::Arc;
        let old = Arc::new(());
        let new = Arc::new(());
        let mut pending = HashMap::from([("same-key".to_owned(), new.clone())]);
        assert!(
            remove_owned_callback(&mut pending, "same-key", |item| Arc::ptr_eq(item, &old))
                .is_none()
        );
        assert!(Arc::ptr_eq(pending.get("same-key").unwrap(), &new));
        assert!(
            remove_owned_callback(&mut pending, "same-key", |item| Arc::ptr_eq(item, &new))
                .is_some()
        );
        assert!(pending.is_empty());
    }

    #[test]
    fn retirement_closes_cards_even_when_cleanup_is_dropped() {
        let gate = RuntimeRetirementGate::default();
        let mut cards = HashMap::<String, ()>::new();
        assert!(gate.close_if(|| true));
        assert_eq!(
            gate.with_active(|| cards.insert("late".to_owned(), ())),
            None
        );
        assert!(cards.is_empty());
        // No reopened gate is offered by a cleanup result or Drop path.
        assert!(!gate.close_if(|| true));
        assert_eq!(gate.with_active(|| ()), None);
    }

    #[test]
    fn old_callback_cannot_publish_resolution_for_reused_card_key() {
        use std::sync::Arc;
        let old = Arc::new(());
        let new = Arc::new(());
        let mut pending = HashMap::from([("same-key".to_owned(), new.clone())]);
        let resolved = Cell::new(0);
        assert!(!finish_owned_callback(
            &mut pending,
            "same-key",
            |item| Arc::ptr_eq(item, &old),
            || resolved.set(resolved.get() + 1)
        ));
        assert_eq!(resolved.get(), 0);
        assert!(Arc::ptr_eq(pending.get("same-key").unwrap(), &new));
        assert!(finish_owned_callback(
            &mut pending,
            "same-key",
            |item| Arc::ptr_eq(item, &new),
            || resolved.set(resolved.get() + 1)
        ));
        assert_eq!(resolved.get(), 1);
        assert!(!finish_owned_callback(
            &mut pending,
            "same-key",
            |_| true,
            || resolved.set(resolved.get() + 1)
        ));
        assert_eq!(resolved.get(), 1);
    }

    #[test]
    fn dropped_cleanup_is_finalized_before_pool_take_and_replacement_seed() {
        use std::sync::Arc;
        struct Runtime {
            gate: RuntimeRetirementGate,
            pending_terminal: Cell<bool>,
        }
        for path in ["evict", "restart", "shutdown", "idle", "rebind"] {
            let runtime = Arc::new(Runtime {
                gate: RuntimeRetirementGate::default(),
                pending_terminal: Cell::new(false),
            });
            let mut sessions = HashMap::from([(path.to_owned(), runtime.clone())]);
            let journal = std::cell::RefCell::new(vec!["started"]);
            assert!(runtime.gate.close_if(|| {
                runtime.pending_terminal.set(true);
                true
            }));
            // The waiting cleanup future is abandoned: only the actual pool
            // take helper gets to consume and persist its pending terminal.
            let finalize = |current: &Arc<Runtime>| {
                assert!(Arc::ptr_eq(current, &runtime));
                if current.pending_terminal.replace(false) {
                    journal.borrow_mut().push("terminal+flush");
                }
            };
            let removed = take_finalized_runtime(&mut sessions, path, finalize).unwrap();
            assert!(Arc::ptr_eq(&removed, &runtime));
            assert_eq!(journal.borrow().as_slice(), ["started", "terminal+flush"]);
            assert!(take_finalized_runtime(&mut sessions, path, finalize).is_none());
            let replacement_seed = journal.borrow().len();
            journal.borrow_mut().push("replacement started");
            assert_eq!(replacement_seed, 2);
            assert_eq!(
                removed
                    .gate
                    .with_active(|| journal.borrow_mut().push("late old event")),
                None
            );
            assert_eq!(journal.borrow().len(), 3);
        }
    }

    #[test]
    fn unexpected_exit_is_dead_and_consumes_one_crash_slot() {
        let lifecycle = RuntimeLifecycle::new(7);
        assert!(lifecycle.is_running());
        assert!(lifecycle.mark_connection_closed());
        assert!(!lifecycle.is_running());
        assert!(lifecycle.take_uncounted_crash());
        assert!(!lifecycle.take_uncounted_crash());
        assert_eq!(lifecycle.generation(), 7);
    }

    #[test]
    fn resolution_reuses_live_generation_and_atomically_removes_dead_generation() {
        let mut sessions = HashMap::from([
            ("live".to_string(), (11_u64, true)),
            ("dead".to_string(), (10_u64, false)),
        ]);

        assert!(matches!(
            resolve_runtime(&mut sessions, "live", |entry| entry.1),
            RuntimeLookup::Reusable((11, true))
        ));
        assert!(sessions.contains_key("live"));

        assert!(matches!(
            resolve_runtime(&mut sessions, "dead", |entry| entry.1),
            RuntimeLookup::Dead((10, false))
        ));
        assert!(!sessions.contains_key("dead"));
        assert!(matches!(
            resolve_runtime(&mut sessions, "dead", |entry| entry.1),
            RuntimeLookup::Missing
        ));
    }

    #[test]
    fn old_generation_exit_cannot_change_replacement_generation() {
        let old = RuntimeLifecycle::new(21);
        let replacement = RuntimeLifecycle::new(22);

        old.mark_connection_closed();

        assert!(!old.is_running());
        assert!(replacement.is_running());
        assert_eq!(replacement.generation(), 22);
    }

    #[test]
    fn expected_shutdown_does_not_consume_crash_budget() {
        let lifecycle = RuntimeLifecycle::new(8);
        lifecycle.begin_expected_shutdown();
        assert!(!lifecycle.mark_connection_closed());
        assert!(!lifecycle.is_running());
        assert!(!lifecycle.take_uncounted_crash());
    }

    #[test]
    fn crash_budget_backs_off_then_stops_a_tight_respawn_loop() {
        let start = Instant::now();
        let mut budget = RuntimeCrashBudget::default();

        budget.record_crash_at(start);
        assert_eq!(budget.admission_at(start), SpawnAdmission::Allowed);

        budget.record_crash_at(start);
        assert_eq!(
            budget.admission_at(start),
            SpawnAdmission::RetryAfter {
                delay: SECOND_RETRY_DELAY,
                budget_exhausted: false,
            }
        );

        let third = start + SECOND_RETRY_DELAY;
        budget.record_crash_at(third);
        assert!(matches!(
            budget.admission_at(third),
            SpawnAdmission::RetryAfter {
                budget_exhausted: true,
                ..
            }
        ));
        assert_eq!(
            budget.admission_at(start + CRASH_WINDOW),
            SpawnAdmission::Allowed
        );
    }
}
