//! SessionStore-owned remote session records. No transport or Tauri dependency.

pub(crate) mod binding;
mod migration;
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
}

#[cfg(test)]
mod tests;
