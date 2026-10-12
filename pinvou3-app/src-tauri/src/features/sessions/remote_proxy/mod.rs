//! SessionStore-owned remote session records. No transport or Tauri dependency.

mod archive;
mod archive_lifecycle;
pub(crate) mod binding;
pub(crate) mod history;
pub(crate) mod outbox;
mod persistence;
pub(crate) mod projection;
mod records;
pub(crate) mod routing;
pub(crate) mod schema;

use std::collections::HashMap;
use std::path::PathBuf;

use binding::LiveRemoteBinding;
use schema::RemoteProxyRecord;

#[derive(Default)]
pub(crate) struct RemoteProxyState {
    pub(crate) records: HashMap<String, RemoteProxyRecord>,
    /// Unpersisted proof of an open in this process and its original resume boundary.
    pub(super) openings: HashMap<String, LiveRemoteBinding>,
    /// The controller has one current live connection. Historical per-record
    /// bindings cannot authorize writes after re-pair or lease replacement.
    pub(super) current_context: Option<binding::LiveRemoteContext>,
    pub(super) path: PathBuf,
    /// A failed load disables remote writes for this process. Keep the original
    /// file untouched rather than replacing unreadable history with an empty map.
    pub(super) load_failed: bool,
    pub(super) archives: archive::ArchiveStore,
    /// Last successfully durable reservation total. Legacy files may exceed
    /// new admission headroom; recovery writes may reduce, never increase it.
    pub(super) persisted_required: std::cell::Cell<Option<usize>>,
}

#[cfg(test)]
mod tests;
