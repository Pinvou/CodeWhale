pub mod app_events;
pub(crate) mod capabilities;
pub mod connector_lock;
pub mod connector_skills;
pub mod connector_state;
pub mod credential_store;
pub(crate) mod download;
pub(crate) mod encoding;
pub(crate) mod filesystem;
pub(crate) mod hashing;
pub(crate) mod notifications;
pub(crate) mod os;
pub(crate) mod path_policy;
pub mod paths;
pub mod prefs;
pub(crate) mod process;
pub(crate) mod startup;
pub(crate) mod strings;
pub mod super_permission;
pub(crate) mod telemetry;
pub(crate) mod ui_cache;
pub(crate) mod window_startup;

#[cfg(feature = "behavior-telemetry-test")]
#[doc(hidden)]
pub mod behavior_telemetry_test_support {
    use std::io;
    use std::path::Path;

    pub const TELEMETRY_DEFAULT_BASE_URL: &str = super::telemetry::TELEMETRY_DEFAULT_BASE_URL;
    pub const TELEMETRY_ENROLLMENT_TOKEN: &str = super::telemetry::TELEMETRY_ENROLLMENT_TOKEN;

    pub fn atomic_write_private(path: &Path, content: &[u8]) -> io::Result<()> {
        super::filesystem::atomic_write_private(path, content)
    }

    pub fn open_private_append_file(path: &Path) -> io::Result<std::fs::File> {
        super::filesystem::open_private_append_file(path)
    }

    pub fn startup_elapsed_ms() -> u64 {
        super::startup::elapsed_ms()
    }

    pub fn hex_lower(bytes: &[u8]) -> String {
        super::encoding::hex_lower(bytes)
    }
}
