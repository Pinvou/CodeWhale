//! Actual archive SQL, schema validation and OS persistence primitives.
//! No SessionStore, host lifecycle ownership, or Tauri UI is exercised here.
pub(crate) mod binding {
    pub(crate) struct LiveRemoteContext;
}
#[path = "../../../src/features/sessions/remote_proxy/schema.rs"]
pub(crate) mod schema;
#[path = "../../../src/features/sessions/remote_proxy/archive.rs"]
mod archive;
