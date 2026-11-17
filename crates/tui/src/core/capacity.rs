//! Cancellation-safe dynamic semaphore shared by model admission and child scheduling.

use std::sync::Mutex;

// === DynamicGate ===

#[derive(Debug)]
struct GateState {
    capacity: usize,
    outstanding: usize,
}

/// A launch gate with runtime-adjustable capacity (see module docs).
///
/// A thin accounting layer over [`tokio::sync::Semaphore`]: the semaphore
/// owns the FIFO wait queue and cancellation safety, this struct owns the
/// `capacity`/`outstanding` ledger that lets capacity drop below the
/// number of outstanding holders. The ledger — not the semaphore token
/// count — is the admission authority: a granted token is registered only
/// while `outstanding < capacity`; one granted while the ledger is full is
/// surplus and is destroyed (`forget`) instead of admitted. Two paths
/// create such tokens while no slot is free, both invisible to
/// `forget_permits` (which sees free tokens only): a grant that races a
/// capacity change (handed out before the shrink can forget it), and a
/// grant handed to a queued waiter that is cancelled before polling again
/// (tokio re-injects it into the pool). Surplus tokens are harmless — they
/// cannot admit — and the next admission attempt destroys them; every slot
/// freed below capacity mints a fresh token on release, so no slot is ever
/// lost. Once no surplus token remains and every in-flight admission has
/// registered, `sem.available_permits() ==
/// capacity.saturating_sub(outstanding)` holds exactly.
///
/// `acquire` returns a [`DynamicGatePermit`] whose `Drop` returns the slot.
/// Reducing capacity below `outstanding` is allowed: the surplus holders
/// finish naturally and no new permit is granted until the outstanding
/// count drops under the new capacity.
#[derive(Debug)]
pub(crate) struct DynamicGate {
    sem: tokio::sync::Semaphore,
    inner: Mutex<GateState>,
}

impl DynamicGate {
    pub(crate) fn new(capacity: usize) -> Self {
        let capacity = capacity.max(1);
        Self {
            sem: tokio::sync::Semaphore::new(capacity),
            inner: Mutex::new(GateState {
                capacity,
                outstanding: 0,
            }),
        }
    }

    pub(crate) fn capacity(&self) -> usize {
        self.inner.lock().expect("launch gate poisoned").capacity
    }

    /// Free semaphore permits right now — `capacity - outstanding` at
    /// rest, plus any surplus tokens not yet destroyed by an admission
    /// attempt (see the struct docs). Diagnostics and tests only; racy by
    /// design.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn available_permits(&self) -> usize {
        self.sem.available_permits()
    }

    /// Adjust the gate capacity. Raising it adds the new free headroom to
    /// the semaphore immediately (queued acquirers wake in FIFO order);
    /// lowering it forgets free slots — holders above the new capacity keep
    /// running, and their releases are absorbed instead of re-admitted.
    /// Grants already handed to queued waiters are invisible to the shrink;
    /// if their acquirers are cancelled, admission destroys the returned
    /// surplus token (see the struct docs).
    pub(crate) fn set_capacity(&self, capacity: usize) {
        let mut inner = self.inner.lock().expect("launch gate poisoned");
        let old = inner.capacity;
        inner.capacity = capacity;
        if capacity < old {
            // `forget_permits` caps the reduction at the available count, so
            // an over-subscribed shrink (capacity < outstanding) simply
            // drains free slots to zero.
            self.sem.forget_permits(old - capacity);
        } else {
            let free = |cap: usize| cap.saturating_sub(inner.outstanding);
            self.sem.add_permits(free(capacity) - free(old));
        }
    }

    /// Try to acquire a permit without waiting.
    pub(crate) fn try_acquire(self: &std::sync::Arc<Self>) -> Option<DynamicGatePermit> {
        let sem_permit = self.sem.try_acquire().ok()?;
        self.admit_grant(sem_permit)
    }

    /// Register a granted semaphore token as a held slot. The ledger, not
    /// the token count, is the admission authority: a token granted while
    /// the ledger is full is surplus drift — a cancelled waiter's
    /// re-injected grant, or a grant that raced a shrink — and is destroyed
    /// with `forget` (never dropped: a drop would re-inject it). Since
    /// `DynamicGatePermit::drop` mints one fresh token per slot freed below
    /// capacity, destroying a token never loses a slot.
    fn admit_grant(
        self: &std::sync::Arc<Self>,
        sem_permit: tokio::sync::SemaphorePermit<'_>,
    ) -> Option<DynamicGatePermit> {
        let mut inner = self.inner.lock().expect("launch gate poisoned");
        if inner.outstanding < inner.capacity {
            inner.outstanding += 1;
            // Slot ownership moves from the semaphore permit into
            // `DynamicGatePermit::drop`.
            sem_permit.forget();
            return Some(DynamicGatePermit {
                gate: std::sync::Arc::clone(self),
            });
        }
        sem_permit.forget();
        None
    }

    /// Acquire a permit, waiting until capacity is available. The semaphore
    /// queue is FIFO and cancel safe: a cancelled future dequeues itself and
    /// never swallows a slot or loses a wakeup. A grant that turns out to be
    /// surplus (see [`Self::admit_grant`]) is destroyed and the wait
    /// continues for the release that mints a fresh one; each surplus round
    /// consumes exactly one pool token, so the loop cannot spin.
    pub(crate) async fn acquire(self: &std::sync::Arc<Self>) -> DynamicGatePermit {
        loop {
            // The semaphore is never closed, so `acquire` cannot fail.
            let sem_permit = self
                .sem
                .acquire()
                .await
                .expect("launch gate semaphore closed");
            if let Some(permit) = self.admit_grant(sem_permit) {
                return permit;
            }
        }
    }
}

/// One held launch slot. Released on drop.
pub(crate) struct DynamicGatePermit {
    gate: std::sync::Arc<DynamicGate>,
}

impl std::fmt::Debug for DynamicGatePermit {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DynamicGatePermit").finish()
    }
}

impl Drop for DynamicGatePermit {
    fn drop(&mut self) {
        let mut inner = self.gate.inner.lock().expect("launch gate poisoned");
        inner.outstanding = inner.outstanding.saturating_sub(1);
        if inner.outstanding < inner.capacity {
            // Return the slot; a queued acquirer wakes in FIFO order.
            self.gate.sem.add_permits(1);
        }
        // Otherwise the capacity was shrunk below the outstanding count:
        // the release is absorbed so the surplus drain does not over-admit.
    }
}
