//! Standalone behavior-telemetry test harness.
//!
//! These tests intentionally compile separately from the already-large lib
//! test harness. The implementation is included from its single source file,
//! while the small platform surface delegates to the real library adapters.

mod platform {
    pub mod connector_lock {
        use std::path::Path;

        pub fn registered_telemetry_cli(key: &str, program: &str, cwd: &Path) -> bool {
            let installed = cwd.join("managed-fixture").join(super::fixture_cli_name());
            key == "feishu"
                && pinvou3_lib::platform::behavior_telemetry_test_support::matches_installed_connector_launcher(
                    program, cwd, &installed, None,
                )
        }
    }

    #[cfg(windows)]
    pub fn fixture_cli_name() -> &'static str {
        "lark-cli.cmd"
    }

    #[cfg(unix)]
    pub fn fixture_cli_name() -> &'static str {
        "lark-cli"
    }

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
    pub mod behavior_telemetry {
        pub(crate) use crate::behavior_telemetry::connector_cli_key;
    }

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

#[cfg(unix)]
fn synthetic_lark_command(folder: &std::path::Path, exit: i64, managed: bool) -> &'static str {
    use std::os::unix::fs::PermissionsExt;
    let location = if managed {
        folder.join("managed-fixture")
    } else {
        folder.to_path_buf()
    };
    std::fs::create_dir_all(&location).unwrap();
    let executable = location.join(platform::fixture_cli_name());
    std::fs::write(&executable, format!("#!/bin/sh\nexit {exit}\n")).unwrap();
    std::fs::set_permissions(executable, std::fs::Permissions::from_mode(0o700)).unwrap();
    if managed {
        "./managed-fixture/lark-cli"
    } else {
        "./lark-cli"
    }
}

#[cfg(windows)]
fn synthetic_lark_command(folder: &std::path::Path, exit: i64, managed: bool) -> &'static str {
    let location = if managed {
        folder.join("managed-fixture")
    } else {
        folder.to_path_buf()
    };
    std::fs::create_dir_all(&location).unwrap();
    std::fs::write(
        location.join(platform::fixture_cli_name()),
        format!("@exit /b {exit}\r\n"),
    )
    .unwrap();
    if managed {
        "./managed-fixture/lark-cli.cmd"
    } else {
        "./lark-cli.cmd"
    }
}

#[test]
fn installed_launcher_excludes_workspace_shadow_and_missing_program() {
    use pinvou3_lib::platform::behavior_telemetry_test_support::matches_installed_connector_launcher;

    let root = tempfile::tempdir().unwrap();
    let work = root.path().join("workspace");
    let managed = root.path().join("managed");
    let wrong = root.path().join("earlier");
    for directory in [&work, &managed, &wrong] {
        std::fs::create_dir(directory).unwrap();
    }
    let executable = platform::fixture_cli_name();
    let installed = managed.join(executable);
    let path = std::env::join_paths([&wrong, &managed]).unwrap();
    assert!(!matches_installed_connector_launcher(
        "lark-cli",
        &work,
        &installed,
        Some(&path)
    ));
    std::fs::write(&installed, b"fixture").unwrap();
    assert!(matches_installed_connector_launcher(
        "lark-cli",
        &work,
        &installed,
        Some(&path)
    ));
    std::fs::write(wrong.join(executable), b"unrelated fixture").unwrap();
    assert!(!matches_installed_connector_launcher(
        "lark-cli",
        &work,
        &installed,
        Some(&path)
    ));
    std::fs::write(work.join(executable), b"unrelated fixture").unwrap();
    assert!(!matches_installed_connector_launcher(
        &format!("./{executable}"),
        &work,
        &installed,
        Some(&path),
    ));
    assert!(matches_installed_connector_launcher(
        installed.to_str().unwrap(),
        &work,
        &installed,
        Some(&path),
    ));
}

#[tokio::test]
async fn diagnostic_real_engine_default_bash_produces_exact_exit_facts() {
    behavior_telemetry::tests::shell_producer::run_shell_producer_fixture(
        shell_diagnostics::tool_completion_metadata,
        shell_diagnostics::foreground_connector_key,
        synthetic_lark_command,
    )
    .await;
}
