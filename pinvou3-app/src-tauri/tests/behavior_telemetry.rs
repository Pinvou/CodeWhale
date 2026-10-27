//! Standalone behavior-telemetry test harness.
//!
//! These tests intentionally compile separately from the already-large lib
//! test harness. The implementation is included from its single source file,
//! while the small platform surface delegates to the real library adapters.

mod platform {
    pub mod telemetry {
        pub use pinvou3_lib::platform::behavior_telemetry_test_support::{
            TELEMETRY_DEFAULT_BASE_URL, TELEMETRY_ENROLLMENT_TOKEN,
        };
    }

    pub mod filesystem {
        use std::io;
        use std::path::Path;
        pub fn read_private_data_file_bytes_within(
            path: &Path,
            max_bytes: u64,
        ) -> io::Result<Vec<u8>> {
            pinvou3_lib::platform::behavior_telemetry_test_support::read_private_data_file_bytes_within(path, max_bytes)
        }

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

#[path = "../src/features/assistant/shell_diagnostics.rs"]
mod shell_diagnostics;

#[tokio::test]
async fn diagnostic_real_engine_default_bash_produces_exact_exit_facts() {
    behavior_telemetry::tests::shell_producer::run_shell_producer_fixture(
        shell_diagnostics::tool_completion_metadata,
    )
    .await;
}
