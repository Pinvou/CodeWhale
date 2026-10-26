//! LibreOffice's local stream bridge for sealed Linux sources. Regular files
//! continue through the existing converter; other hosts never enter this path.
use std::path::Path;

pub(in crate::features::files) fn dependency() -> Option<(bool, &'static str)> {
    #[cfg(all(target_os = "linux", target_pointer_width = "64"))]
    {
        let mut command = crate::platform::process::HiddenCommand::new("/usr/bin/python3");
        command.args(["-I", "-c", "import uno"]);
        let installed = crate::platform::process::output_with_timeout_and_kill_tree(
            command,
            std::time::Duration::from_secs(10),
        )
        .is_ok_and(|output| output.status.success());
        Some((installed, "python3-uno"))
    }
    #[cfg(not(all(target_os = "linux", target_pointer_width = "64")))]
    {
        None
    }
}

pub(in crate::features::files) fn convert(
    path: &Path,
    conversion: &str,
    output_directory: &Path,
) -> Option<Result<(), String>> {
    #[cfg(all(target_os = "linux", target_pointer_width = "64"))]
    {
        // Only a process-owned fd source uses this adapter. The Python helper
        // independently requires all four irreversible seals before reading.
        if path.parent() != Some(Path::new(&format!("/proc/{}/fd", std::process::id()))) {
            return None;
        }
        if !Path::new("/usr/bin/python3").is_file() {
            return Some(Err(
                crate::features::files::file_ingest::ingest_diagnostics::OFFICE_STREAM_MISSING
                    .into(),
            ));
        }
        let mut command = crate::platform::process::HiddenCommand::new("/usr/bin/python3");
        command
            .args(["-I", "-c", include_str!("office_stream.py")])
            .arg(path)
            .arg(output_directory)
            .arg(conversion)
            .arg(crate::platform::os::libreoffice_tool_path())
            .arg(crate::features::files::limits::MAX_FILE_BYTES.to_string());
        Some(
            crate::platform::process::output_with_timeout_and_kill_tree(
                command,
                std::time::Duration::from_secs(180),
            )
            .map_err(|_| {
                crate::features::files::file_ingest::ingest_diagnostics::CONVERSION_FAILED
                    .to_owned()
            })
            .and_then(|output| {
                let expected = output_directory.join(format!(
                    "{}.{}",
                    path.file_stem().unwrap_or_default().to_string_lossy(),
                    conversion.split(':').next().unwrap_or_default(),
                ));
                checked_output(output.status.code(), &expected)
            }),
        )
    }
    #[cfg(not(all(target_os = "linux", target_pointer_width = "64")))]
    {
        let _ = (path, conversion, output_directory);
        None
    }
}

#[cfg(all(target_os = "linux", target_pointer_width = "64"))]
fn checked_output(code: Option<i32>, expected: &Path) -> Result<(), String> {
    use crate::features::files::file_ingest::ingest_diagnostics::{
        CONVERSION_FAILED, OFFICE_STREAM_MISSING,
    };
    match code {
        Some(0) if expected.is_file() => Ok(()),
        Some(78) => Err(OFFICE_STREAM_MISSING.into()),
        _ => Err(CONVERSION_FAILED.into()),
    }
}

#[cfg(all(test, target_os = "linux", target_pointer_width = "64"))]
mod tests {
    use super::*;

    #[test]
    fn converter_requires_real_output_and_identifies_the_missing_dependency() {
        let root = tempfile::tempdir().unwrap();
        let output = root.path().join("report.txt");
        assert_eq!(
            checked_output(Some(0), &output).unwrap_err(),
            "attachment_conversion_failed"
        );
        assert_eq!(
            checked_output(Some(78), &output).unwrap_err(),
            "attachment_office_stream_dependency_missing"
        );
        std::fs::write(&output, b"converted").unwrap();
        assert!(checked_output(Some(0), &output).is_ok());
        assert!(checked_output(Some(1), &output).is_err());
    }

    #[test]
    #[ignore = "requires local /usr/bin/python3; no UNO, network or model"]
    fn helper_startup_budget_and_missing_uno_are_deterministic() {
        let script = r#"
import sys
namespace = {'__name__': 'office_stream_test'}
exec(sys.argv[1], namespace)
class Process:
    def poll(self): return None
class Resolver:
    def __init__(self, success_after): self.calls, self.success_after = 0, success_after
    def resolve(self, url):
        self.calls += 1
        if self.calls <= self.success_after: raise RuntimeError('synthetic cold start')
        return 'ready'
ticks = iter([0, 21, 59])
assert namespace['wait_for_context'](Resolver(2), 'test', Process(), lambda: next(ticks), lambda _: None) == 'ready'
ticks = iter([0, 61])
try:
    namespace['wait_for_context'](Resolver(99), 'test', Process(), lambda: next(ticks), lambda _: None)
except RuntimeError:
    pass
else:
    raise AssertionError('startup exceeded the 60 second budget')
"#;
        let mut command = crate::platform::process::HiddenCommand::new("/usr/bin/python3");
        command.args(["-I", "-c", script, include_str!("office_stream.py")]);
        let output = crate::platform::process::output_with_timeout_and_kill_tree(
            command,
            std::time::Duration::from_secs(10),
        )
        .unwrap();
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let mut command = crate::platform::process::HiddenCommand::new("/usr/bin/python3");
        command.args(["-I", "-S", "-c", include_str!("office_stream.py")]);
        let output = crate::platform::process::output_with_timeout_and_kill_tree(
            command,
            std::time::Duration::from_secs(10),
        )
        .unwrap();
        assert_eq!(output.status.code(), Some(78));
        assert_eq!(
            String::from_utf8_lossy(&output.stderr).trim(),
            "office_stream_dependency_missing"
        );
    }

    #[test]
    #[ignore = "requires local LibreOffice and python3-uno; no network or model"]
    fn sealed_rtf_converts_through_private_stream_without_a_named_input() {
        let root = tempfile::tempdir().unwrap();
        let body = b"{\\rtf1\\ansi Verified sealed RTF input.}";
        let owner = crate::platform::filesystem::publish_private_read_path(
            crate::platform::filesystem::open_private_file_directory(
                &root.path().join("remote_uploads/ctr-up-office"),
            )
            .unwrap(),
            std::ffi::OsStr::new("report.rtf"),
            body,
        )
        .unwrap();
        let output = root.path().join("output");
        std::fs::create_dir(&output).unwrap();
        convert(owner.read_path(), "txt:Text (encoded):UTF8", &output)
            .unwrap()
            .unwrap();
        let stem = owner.read_path().file_stem().unwrap().to_string_lossy();
        let text = std::fs::read_to_string(output.join(format!("{stem}.txt"))).unwrap();
        assert!(text.contains("Verified sealed RTF input."));
        assert!(!owner.path().exists());
        use sha2::{Digest as _, Sha256};
        let owner = std::sync::Arc::new(owner);
        let descriptor = crate::features::files::file_ingest::PublishedAttachment {
            handle_id: "ctr-up-office".into(),
            path: owner.path().to_owned(),
            display_name: "report.rtf".into(),
            mime_type: "application/rtf".into(),
            bytes: body.len() as u64,
            sha256: crate::platform::encoding::hex_lower(&Sha256::digest(body)),
            read_owner: Some(owner),
        };
        let ingested =
            crate::features::files::file_ingest::ingest_staged_attachment(root.path(), &descriptor)
                .unwrap();
        assert_eq!(ingested.kind, "rtf");
        assert!(
            ingested
                .markdown
                .as_deref()
                .unwrap()
                .contains("Verified sealed RTF input.")
        );
    }
}
