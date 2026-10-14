//! Pure admission policy. An observed RSS is not a certified peak budget.

#[derive(Debug, PartialEq, Eq)]
pub struct Decision {
    pub level: &'static str,
    pub reason: &'static str,
    pub confirmation: bool,
    pub projected_available: Option<u64>,
}

impl Decision {
    pub fn sampling_unavailable() -> Self {
        Self {
            level: "unknown",
            reason: "measurement_unavailable",
            confirmation: true,
            projected_available: None,
        }
    }
}

pub fn evaluate(
    available: u64,
    safety_reserve: u64,
    hard_reserve: u64,
    candidate_peak: Option<u64>,
    running_count: usize,
    running_growth: Option<u64>,
) -> Decision {
    let candidate_peak = candidate_peak.filter(|peak| *peak > 0);
    let projected = candidate_peak
        .zip(running_growth)
        .map(|(peak, growth)| available.saturating_sub(peak).saturating_sub(growth));
    if available < hard_reserve || projected.is_some_and(|left| left < hard_reserve) {
        return Decision {
            level: "warning",
            reason: "insufficient",
            confirmation: true,
            projected_available: projected,
        };
    }
    if projected.is_some_and(|left| left < safety_reserve) {
        return Decision {
            level: "caution",
            reason: "limited",
            confirmation: true,
            projected_available: projected,
        };
    }
    if projected.is_some() {
        return Decision {
            level: "green",
            reason: "sufficient",
            confirmation: false,
            projected_available: projected,
        };
    }
    // The first model is allowed without a missing-metadata prompt, but its
    // unknown budget must never be described as a proven green result.
    let first_verified_empty = running_count == 0 && running_growth.is_some();
    Decision {
        level: "unknown",
        reason: if first_verified_empty {
            "first_unverified"
        } else {
            "unverified"
        },
        confirmation: !first_verified_empty,
        projected_available: None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn failed_sampling_requests_explicit_consent_without_inventing_zero() {
        let result = Decision::sampling_unavailable();
        assert!(result.confirmation);
        assert_eq!(result.level, "unknown");
        assert_eq!(result.projected_available, None);
    }

    #[test]
    fn first_unknown_is_allowed_but_not_claimed_safe() {
        let result = evaluate(90, 15, 10, None, 0, Some(0));
        assert!(!result.confirmation);
        assert_eq!(result.reason, "first_unverified");
        assert_eq!(result.projected_available, None);
    }

    #[test]
    fn first_model_still_warns_on_real_pressure_without_hard_blocking() {
        assert!(evaluate(9, 15, 10, None, 0, Some(0)).confirmation);
        assert_eq!(
            evaluate(90, 15, 10, Some(100), 0, Some(0)).reason,
            "insufficient"
        );
    }

    #[test]
    fn subsequent_models_require_complete_budgets() {
        assert!(evaluate(90, 15, 10, None, 1, Some(0)).confirmation);
        assert!(evaluate(90, 15, 10, Some(20), 1, None).confirmation);
        assert!(!evaluate(90, 15, 10, Some(20), 1, Some(10)).confirmation);
        // A scan timeout cannot silently turn a second model into a first one.
        let incomplete = evaluate(90, 15, 10, Some(20), 0, None);
        assert!(incomplete.confirmation);
        assert_eq!(incomplete.reason, "unverified");
    }

    #[test]
    fn reserves_existing_models_future_growth() {
        let result = evaluate(50, 15, 10, Some(20), 1, Some(25));
        assert_eq!(result.projected_available, Some(5));
        assert_eq!(result.reason, "insufficient");
        assert_eq!(
            evaluate(50, 15, 10, Some(20), 1, Some(18)).reason,
            "limited"
        );
    }

    #[test]
    fn missing_or_zero_peak_is_not_free_memory() {
        assert!(evaluate(90, 15, 10, Some(0), 1, Some(0)).confirmation);
        assert_eq!(
            evaluate(90, 15, 10, Some(u64::MAX), 1, Some(u64::MAX)).projected_available,
            Some(0)
        );
    }
}
