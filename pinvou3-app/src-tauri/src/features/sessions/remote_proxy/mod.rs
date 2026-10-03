//! SessionStore-owned remote session records. No transport or Tauri dependency.

pub(crate) mod binding;
pub(crate) mod history;
pub(crate) mod outbox;
mod persistence;
pub(crate) mod projection;
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
}

#[cfg(test)]
mod tests;
