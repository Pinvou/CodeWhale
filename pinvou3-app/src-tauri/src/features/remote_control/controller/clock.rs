//! One local clock domain for remote-control deadlines across command adapters.

use std::sync::OnceLock;
use std::time::Instant;

pub(crate) fn monotonic_millis() -> u64 {
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    elapsed_millis(ORIGIN.get_or_init(Instant::now), Instant::now())
}

fn elapsed_millis(origin: &Instant, now: Instant) -> u64 {
    u64::try_from(now.saturating_duration_since(*origin).as_millis()).unwrap_or(u64::MAX)
}

#[cfg(test)]
mod tests {
    use super::super::capabilities::{CapabilitySnapshotManager, CapabilitySource};
    use super::super::protocol::LeaseContext;
    use super::*;

    #[test]
    fn delayed_consumer_keeps_the_producers_time_origin() {
        let origin = Instant::now();
        let produced = elapsed_millis(&origin, origin + std::time::Duration::from_secs(7));
        let consumed = elapsed_millis(&origin, origin + std::time::Duration::from_secs(37));
        assert_eq!(produced, 7_000);
        assert_eq!(consumed, 37_000);
        assert!(consumed >= produced + 30_000);
    }

    #[test]
    fn snapshot_admission_expires_at_the_shared_absolute_boundary() {
        let origin = Instant::now();
        let lease = LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-0001".into(),
            remote_control_generation: "generation-0001".into(),
        };
        let snapshots = CapabilitySnapshotManager::default();
        let at = |ms| elapsed_millis(&origin, origin + std::time::Duration::from_millis(ms));
        let published = snapshots
            .build(lease.clone(), CapabilitySource::default(), at(7_000))
            .unwrap();
        assert!(snapshots.validate("1", &lease, at(36_999)).is_ok());
        for now in [37_000, 50_000] {
            assert_eq!(
                snapshots.validate("1", &lease, at(now)).unwrap_err().code,
                "capability_snapshot_expired"
            );
        }
        assert_eq!(
            serde_json::to_value(published).unwrap()["expiresAtMs"],
            "37000"
        );
    }
}
