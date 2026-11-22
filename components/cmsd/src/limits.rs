//! Bounded connection/stream lifetimes, including panic and disconnect paths.
use std::sync::{
    atomic::{AtomicUsize, Ordering},
    Arc,
};

// Use the stable compare-exchange primitive so both older toolchains and newer
// toolchains that renamed fetch_update enforce the same bounded reservation.
fn try_reserve(counter: &AtomicUsize, amount: usize, maximum: usize) -> bool {
    let mut current = counter.load(Ordering::Acquire);
    loop {
        let Some(next) = current.checked_add(amount).filter(|next| *next <= maximum) else {
            return false;
        };
        match counter.compare_exchange_weak(current, next, Ordering::AcqRel, Ordering::Acquire) {
            Ok(_) => return true,
            Err(observed) => current = observed,
        }
    }
}

pub struct Slots {
    active: AtomicUsize,
    maximum: usize,
}
pub struct Permit(Arc<Slots>);
impl Slots {
    pub fn new(maximum: usize) -> Arc<Self> {
        Arc::new(Self {
            active: AtomicUsize::new(0),
            maximum,
        })
    }
    pub fn acquire(self: &Arc<Self>) -> Option<Permit> {
        if !try_reserve(&self.active, 1, self.maximum) {
            return None;
        }
        Some(Permit(self.clone()))
    }
}
impl Drop for Permit {
    fn drop(&mut self) {
        self.0.active.fetch_sub(1, Ordering::AcqRel);
    }
}

/// Separate small-message capacity keeps cancellation/health and ordinary
/// inference available while large uploads occupy their own bounded pool.
#[derive(Debug)]
pub struct BodyBudget {
    small: AtomicUsize,
    large: AtomicUsize,
    small_max: usize,
    large_max: usize,
    cutoff: usize,
}
#[derive(Debug)]
pub struct BodyPermit {
    budget: Arc<BodyBudget>,
    held: usize,
    large: bool,
}
impl BodyBudget {
    pub fn new(small_max: usize, large_max: usize, cutoff: usize) -> Arc<Self> {
        Arc::new(Self {
            small: AtomicUsize::new(0),
            large: AtomicUsize::new(0),
            small_max,
            large_max,
            cutoff,
        })
    }
    pub fn permit(self: &Arc<Self>) -> BodyPermit {
        BodyPermit {
            budget: self.clone(),
            held: 0,
            large: false,
        }
    }
}
impl BodyPermit {
    pub fn reserve(&mut self, total: usize) -> bool {
        if total < self.held {
            return false;
        }
        let large = self.large || total > self.budget.cutoff;
        let (counter, maximum) = if large {
            (&self.budget.large, self.budget.large_max)
        } else {
            (&self.budget.small, self.budget.small_max)
        };
        let changed = large != self.large;
        let delta = if changed { total } else { total - self.held };
        if !try_reserve(counter, delta, maximum) {
            return false;
        }
        if changed {
            self.budget.small.fetch_sub(self.held, Ordering::AcqRel);
        }
        self.large = large;
        self.held = total;
        true
    }
}
impl Drop for BodyPermit {
    fn drop(&mut self) {
        let counter = if self.large {
            &self.budget.large
        } else {
            &self.budget.small
        };
        counter.fetch_sub(self.held, Ordering::AcqRel);
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn admission_and_unwind_release_bounded_slots() {
        let slots = Slots::new(2);
        let first = slots.acquire().unwrap();
        let second = slots.acquire().unwrap();
        assert!(slots.acquire().is_none());
        drop(first);
        let _ = std::panic::catch_unwind(|| {
            let _held = slots.acquire().unwrap();
            panic!("injected disconnect");
        });
        assert!(slots.acquire().is_some());
        drop(second);
        assert_eq!(slots.active.load(Ordering::Acquire), 0);
    }

    #[test]
    fn body_pools_keep_small_capacity_and_release_failed_growth() {
        let budget = BodyBudget::new(8, 16, 4);
        let mut large = budget.permit();
        assert!(large.reserve(16));
        let mut small = budget.permit();
        assert!(small.reserve(4));
        assert!(!small.reserve(5));
        assert_eq!(budget.small.load(Ordering::Acquire), 4);
        assert_eq!(budget.large.load(Ordering::Acquire), 16);
        drop(large);
        assert!(small.reserve(5));
        assert_eq!(budget.small.load(Ordering::Acquire), 0);
        assert_eq!(budget.large.load(Ordering::Acquire), 5);
        drop(small);
        assert_eq!(budget.large.load(Ordering::Acquire), 0);
    }
}
