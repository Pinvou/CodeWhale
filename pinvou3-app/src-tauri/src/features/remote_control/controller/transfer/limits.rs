//! Transfer resource budgets (DEV-14).
//!
//! No controller limit may be wider than the existing Web transfer limits:
//! Web attachments already cap a single file at 32 MiB, the whole upload set at
//! 64 MiB and in-flight uploads at 4. The controller core reuses those numbers
//! and adds a chunk ceiling taken from the frozen protocol constants.

use std::time::Duration;

pub(crate) use super::super::protocol::MAX_TRANSFER_CHUNK_BYTES;

pub(crate) const MAX_TRANSFER_BYTES: u64 = 32 * 1024 * 1024;
pub(crate) const MAX_CONNECTION_TRANSFER_BYTES: u64 = 64 * 1024 * 1024;
pub(crate) const MAX_IN_FLIGHT_TRANSFERS: usize = 4;
pub(crate) const MAX_TRANSFERS_PER_TASK: usize = 8;
pub(crate) const TRANSFER_TTL: Duration = Duration::from_secs(10 * 60);
pub(crate) const MAX_TRANSFER_ID_CHARS: usize = 128;
pub(crate) const MAX_FILE_NAME_CHARS: usize = 200;
pub(crate) const MAX_MIME_CHARS: usize = 120;

/// Effective budgets. The defaults match the constants above; a deployment may
/// only *narrow* them.
#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) struct TransferLimits {
    pub(crate) max_transfer_bytes: u64,
    pub(crate) max_connection_bytes: u64,
    pub(crate) max_in_flight: usize,
    pub(crate) max_transfers_per_task: usize,
    pub(crate) chunk_bytes: usize,
    pub(crate) ttl: Duration,
}

impl Default for TransferLimits {
    fn default() -> Self {
        Self {
            max_transfer_bytes: MAX_TRANSFER_BYTES,
            max_connection_bytes: MAX_CONNECTION_TRANSFER_BYTES,
            max_in_flight: MAX_IN_FLIGHT_TRANSFERS,
            max_transfers_per_task: MAX_TRANSFERS_PER_TASK,
            chunk_bytes: MAX_TRANSFER_CHUNK_BYTES,
            ttl: TRANSFER_TTL,
        }
    }
}

impl TransferLimits {
    /// Clamps every field to the hard ceiling. Returns `false` when the caller
    /// asked for something wider than allowed, so the wiring layer can fail
    /// closed instead of silently narrowing a security budget.
    pub(crate) fn narrow(self) -> Option<Self> {
        let defaults = Self::default();
        if self.max_transfer_bytes == 0
            || self.max_transfer_bytes > defaults.max_transfer_bytes
            || self.max_connection_bytes < self.max_transfer_bytes
            || self.max_connection_bytes > defaults.max_connection_bytes
            || self.max_in_flight == 0
            || self.max_in_flight > defaults.max_in_flight
            || self.max_transfers_per_task == 0
            || self.max_transfers_per_task > defaults.max_transfers_per_task
            || self.chunk_bytes == 0
            || self.chunk_bytes > defaults.chunk_bytes
            || self.ttl.is_zero()
            || self.ttl > defaults.ttl
        {
            return None;
        }
        Some(self)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_are_bounded_by_the_web_limits() {
        let limits = TransferLimits::default();
        assert_eq!(limits.max_transfer_bytes, 32 * 1024 * 1024);
        assert_eq!(limits.max_connection_bytes, 64 * 1024 * 1024);
        assert_eq!(limits.max_in_flight, 4);
        assert_eq!(limits.chunk_bytes, MAX_TRANSFER_CHUNK_BYTES);
        assert!(limits.narrow().is_some());
    }

    #[test]
    fn wider_budgets_are_refused_instead_of_silently_narrowed() {
        let mut limits = TransferLimits::default();
        limits.max_transfer_bytes *= 2;
        assert!(limits.narrow().is_none());

        let mut limits = TransferLimits::default();
        limits.chunk_bytes *= 2;
        assert!(limits.narrow().is_none());

        let mut limits = TransferLimits::default();
        limits.max_in_flight += 1;
        assert!(limits.narrow().is_none());

        let mut limits = TransferLimits::default();
        limits.max_connection_bytes = limits.max_transfer_bytes / 2;
        assert!(limits.narrow().is_none());

        let mut limits = TransferLimits::default();
        limits.ttl = Duration::from_secs(3600);
        assert!(limits.narrow().is_none());
    }

    #[test]
    fn narrower_budgets_are_accepted() {
        let limits = TransferLimits {
            max_transfer_bytes: 1024,
            max_connection_bytes: 4096,
            max_in_flight: 1,
            max_transfers_per_task: 1,
            chunk_bytes: 256,
            ttl: Duration::from_secs(5),
        };
        assert_eq!(limits.narrow(), Some(limits));
    }
}
