//! Standalone behavior-telemetry test harness.
//!
//! These tests intentionally compile separately from the already-large lib
//! test harness. The implementation is included from its single source file,
//! while the small platform surface delegates to the real library adapters.

mod platform {
    pub mod filesystem {
        use std::io;
        use std::path::Path;

        pub fn atomic_write_private(path: &Path, content: &[u8]) -> io::Result<()> {
            pinvou3_lib::platform::behavior_telemetry_test_support::atomic_write_private(
                path, content,
            )
        }

        pub fn open_private_append_file(path: &Path) -> io::Result<std::fs::File> {
            pinvou3_lib::platform::behavior_telemetry_test_support::open_private_append_file(path)
        }
    }

    pub mod paths {
        pub fn pinvou3_home() -> std::path::PathBuf {
            pinvou3_lib::platform::paths::pinvou3_home()
        }

        pub fn settings_path() -> std::path::PathBuf {
            pinvou3_lib::platform::paths::settings_path()
        }

        pub mod tests {
            pub static ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
        }
    }

    pub mod startup {
        pub fn elapsed_ms() -> u64 {
            pinvou3_lib::platform::behavior_telemetry_test_support::startup_elapsed_ms()
        }
    }

    pub mod encoding {
        pub fn hex_lower(bytes: &[u8]) -> String {
            pinvou3_lib::platform::behavior_telemetry_test_support::hex_lower(bytes)
        }
    }
}

mod features {
    pub mod assistant {
        pub mod platform {
            pub mod bridge {
                pub struct EffectiveModel {
                    pub preset: String,
                    pub model: String,
                }

                pub struct Pinvou3Bridge {
                    pub session_model: Option<()>,
                }

                impl Pinvou3Bridge {
                    pub fn effective_model_owned(&self) -> Option<EffectiveModel> {
                        None
                    }
                }
            }
        }
    }
}

#[path = "../src/features/behavior_telemetry.rs"]
mod behavior_telemetry;
