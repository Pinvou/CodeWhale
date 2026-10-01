//! One local monotonic liveness cycle. Relay timestamps are never inputs.
//! Transport admission is only a local upper bound; Relay and B independently
//! enforce their actually authenticated received-evidence cutoffs.

use std::time::Duration;

const TOTAL_BUDGET: Duration = Duration::from_secs(30);

#[derive(Debug)]
pub(crate) struct LivenessBudget {
    cutoff: Option<u64>,
    frozen: bool,
    expires_at: tokio::time::Instant,
}

impl LivenessBudget {
    pub(crate) fn authenticated_cycle(now_a: u64) -> Self {
        let cutoff = now_a.checked_add(TOTAL_BUDGET.as_millis() as u64);
        Self {
            cutoff,
            frozen: false,
            expires_at: tokio::time::Instant::now()
                + if cutoff.is_some() {
                    TOTAL_BUDGET
                } else {
                    Duration::ZERO
                },
        }
    }

    pub(crate) fn submitted(&mut self, now_a: u64, admitted: bool) {
        if admitted && !self.frozen && self.cutoff.is_some_and(|cutoff| now_a < cutoff) {
            self.cutoff = now_a.checked_add(TOTAL_BUDGET.as_millis() as u64);
            self.expires_at = tokio::time::Instant::now()
                + if self.cutoff.is_some() {
                    TOTAL_BUDGET
                } else {
                    Duration::ZERO
                };
        }
    }

    pub(crate) fn freeze(&mut self, now_a: u64) -> Option<u64> {
        self.frozen = true;
        self.cutoff.filter(|cutoff| now_a < *cutoff)
    }

    pub(crate) fn timer(&self) -> tokio::time::Instant {
        self.expires_at
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test(start_paused = true)]
    async fn first_disconnect_keeps_only_remaining_budget_even_after_retries_and_new_heartbeats() {
        let mut budget = LivenessBudget::authenticated_cycle(2_000);
        let deadline = budget.timer();
        tokio::time::advance(Duration::from_secs(15)).await;
        assert_eq!(budget.freeze(17_000), Some(32_000));
        assert_eq!(
            deadline - tokio::time::Instant::now(),
            Duration::from_secs(15)
        );
        tokio::time::advance(Duration::from_secs(5)).await;
        budget.submitted(22_000, true);
        assert_eq!(budget.freeze(22_000), Some(32_000));
        assert_eq!(budget.timer(), deadline);
        assert_eq!(
            budget.timer() - tokio::time::Instant::now(),
            Duration::from_secs(10)
        );
        tokio::time::advance(Duration::from_secs(10)).await;
        assert_eq!(budget.freeze(32_000), None);
        assert!(budget.timer() <= tokio::time::Instant::now());
    }

    #[tokio::test(start_paused = true)]
    async fn failed_admission_and_expired_evidence_cannot_renew_but_complete_activation_can() {
        let mut budget = LivenessBudget::authenticated_cycle(2_000);
        let first = budget.timer();
        tokio::time::advance(Duration::from_secs(8)).await;
        budget.submitted(10_000, false);
        assert_eq!(budget.timer(), first);
        assert_eq!(budget.cutoff, Some(32_000));
        budget.submitted(10_000, true);
        assert_eq!(budget.cutoff, Some(40_000));
        assert_eq!(budget.timer() - tokio::time::Instant::now(), TOTAL_BUDGET);
        tokio::time::advance(TOTAL_BUDGET).await;
        budget.submitted(40_000, true);
        assert_eq!(budget.cutoff, Some(40_000));
        assert_eq!(budget.freeze(40_000), None);
        let new_cycle = LivenessBudget::authenticated_cycle(40_000);
        assert_eq!(new_cycle.cutoff, Some(70_000));
        assert_eq!(
            new_cycle.timer() - tokio::time::Instant::now(),
            TOTAL_BUDGET
        );
        let overflow = LivenessBudget::authenticated_cycle(u64::MAX);
        assert_eq!(overflow.cutoff, None);
        assert!(overflow.timer() <= tokio::time::Instant::now());
    }
}
