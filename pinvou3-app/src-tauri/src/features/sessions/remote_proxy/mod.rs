//! SessionStore-owned remote session records. No transport or Tauri dependency.

mod archive;
mod archive_lifecycle;
mod archive_snapshot;
pub(crate) mod binding;
pub(crate) mod history;
pub(crate) mod outbox;
mod persistence;
pub(crate) mod projection;
mod projection_reason;
mod records;
pub(crate) mod routing;
pub(crate) mod schema;

use std::collections::HashMap;
use std::path::PathBuf;

use binding::LiveRemoteBinding;
use schema::RemoteProxyRecord;

/// Stable, non-sensitive failure categories; never expose paths or file bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RemoteProxyLoadFailure {
    Unreadable,
    Oversized,
    Malformed,
    UnsupportedVersion,
    InvalidRecords,
    ArchiveUnavailable,
}

impl std::fmt::Display for RemoteProxyLoadFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Unreadable => "remote_proxy_unreadable",
            Self::Oversized => "remote_proxy_oversized",
            Self::Malformed => "remote_proxy_malformed",
            Self::UnsupportedVersion => "remote_proxy_version_unsupported",
            Self::InvalidRecords => "remote_proxy_invalid_records",
            Self::ArchiveUnavailable => "remote_proxy_archive_unavailable",
        })
    }
}

impl std::error::Error for RemoteProxyLoadFailure {}

#[derive(Default)]
pub(crate) struct RemoteProxyState {
    pub(crate) records: HashMap<String, RemoteProxyRecord>,
    /// Unpersisted proof of an open in this process and its original resume boundary.
    pub(super) openings: HashMap<String, LiveRemoteBinding>,
    /// The controller has one current live connection. Historical per-record
    /// bindings cannot authorize writes after re-pair or lease replacement.
    pub(super) current_context: Option<binding::LiveRemoteContext>,
    pub(super) path: PathBuf,
    /// Failed load or indeterminate seal disables protected remote mutations
    /// for this process. Preserve the original files and durable recovery evidence.
    pub(super) load_failed: bool,
    pub(super) load_failure: Option<RemoteProxyLoadFailure>,
    pub(super) archives: archive::ArchiveStore,
    /// Last successfully durable reservation total. Legacy files may exceed
    /// new admission headroom; recovery writes may reduce, never increase it.
    pub(super) persisted_required: std::cell::Cell<Option<usize>>,
}

#[cfg(test)]
mod tests;

#[cfg(test)]
mod quarantine_tests;
