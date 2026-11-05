use crate::features::files::file_ingest::{
    IngestResult, PublishedAttachment, ingest_staged_attachment,
};
use sha2::{Digest as _, Sha256};

fn publish(
    root: &std::path::Path,
    name: &str,
    bytes: &[u8],
) -> (
    std::sync::Arc<crate::platform::filesystem::PublishedPrivateFile>,
    PublishedAttachment,
) {
    let identity = format!("ctr-up-{}", uuid::Uuid::new_v4().simple());
    let directory = crate::platform::filesystem::open_private_file_directory(
        &root.join("remote_uploads").join(&identity),
    )
    .unwrap();
    let owner = std::sync::Arc::new(
        crate::platform::filesystem::publish_private_read_path(
            directory,
            std::ffi::OsStr::new(name),
            bytes,
        )
        .unwrap(),
    );
    owner.request_cleanup();
    let descriptor = PublishedAttachment {
        handle_id: identity,
        path: owner.path().to_owned(),
        display_name: name.into(),
        mime_type: "application/octet-stream".into(),
        bytes: bytes.len() as u64,
        sha256: crate::platform::encoding::hex_lower(&Sha256::digest(bytes)),
        read_owner: Some(owner.clone()),
    };
    (owner, descriptor)
}

#[test]
fn model_references_survive_turn_cleanup_and_fd_number_reuse() {
    const PROBE: &str = "PINVOU_TEST_FD_REFERENCE_PROBE";
    if std::env::var(PROBE).as_deref() != Ok("1") {
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "features::files::platform::linux_staged_attachment_tests::model_references_survive_turn_cleanup_and_fd_number_reuse", "--nocapture"])
            .env(PROBE, "1").output().unwrap();
        assert!(
            output.status.success()
                && String::from_utf8_lossy(&output.stdout).contains("1 passed;"),
            "{} {}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
        return;
    }
    use std::io::Write as _;
    use std::os::fd::{AsRawFd as _, FromRawFd as _};
    let root = tempfile::tempdir().unwrap();
    let bytes = "verified large attachment line\n"
        .repeat(10_000)
        .into_bytes();
    let (owner, descriptor) = publish(root.path(), "large.txt", &bytes);
    let parser_path = owner.read_path().to_owned();
    let parser_fd: i32 = parser_path
        .file_name()
        .unwrap()
        .to_str()
        .unwrap()
        .parse()
        .unwrap();
    let ingested = ingest_staged_attachment(root.path(), &descriptor).unwrap();
    let model_path = std::path::PathBuf::from(&ingested.path);
    assert_ne!(model_path, parser_path);
    let serialized = serde_json::to_string(&ingested).unwrap();
    assert!(serialized.contains(model_path.to_str().unwrap()));
    assert!(!serialized.contains(parser_path.to_str().unwrap()));
    drop(ingested);
    drop(descriptor);
    drop(owner);
    assert!(!parser_path.exists());

    // Isolated process: reserve the released descriptor without overwriting an
    // existing fd or touching any other test's process-owned handles.
    let mut unrelated = tempfile::tempfile().unwrap();
    unrelated
        .write_all(b"unrelated synthetic private content")
        .unwrap();
    let duplicate = if unrelated.as_raw_fd() == parser_fd {
        None
    } else {
        // SAFETY: source is owned and live; F_DUPFD_CLOEXEC allocates a new fd,
        // never closes an existing one. The returned fd is adopted once below.
        let fd = unsafe { libc::fcntl(unrelated.as_raw_fd(), libc::F_DUPFD_CLOEXEC, parser_fd) };
        assert!(fd >= 0);
        // SAFETY: fd is a newly allocated owning descriptor from fcntl.
        let file = unsafe { std::fs::File::from_raw_fd(fd) };
        assert_eq!(file.as_raw_fd(), parser_fd);
        Some(file)
    };
    assert_eq!(
        std::fs::read(&parser_path).unwrap(),
        b"unrelated synthetic private content"
    );
    assert_eq!(std::fs::read(&model_path).unwrap(), bytes);
    std::fs::remove_file(&model_path).unwrap();
    assert_eq!(
        std::fs::read(&model_path).unwrap_err().kind(),
        std::io::ErrorKind::NotFound
    );
    drop(duplicate);
}

#[test]
fn closed_sources_are_unavailable_but_scope_and_digest_mismatches_remain_invalid() {
    let root = tempfile::tempdir().unwrap();
    let (owner, descriptor) = publish(root.path(), "text.txt", b"verified");
    let mut invalid = descriptor.clone();
    invalid.sha256 = "0".repeat(64);
    assert_eq!(
        ingest_staged_attachment(root.path(), &invalid).unwrap_err(),
        "task_attachment_invalid"
    );
    let folder = owner.path().parent().unwrap();
    let retired = folder.with_extension("retired");
    std::fs::rename(folder, &retired).unwrap();
    std::fs::create_dir(folder).unwrap();
    assert_eq!(
        ingest_staged_attachment(root.path(), &descriptor).unwrap_err(),
        "task_attachment_invalid"
    );
    std::fs::remove_dir(folder).unwrap();
    std::fs::rename(retired, folder).unwrap();
    owner.cleanup().unwrap();
    assert_eq!(
        ingest_staged_attachment(root.path(), &descriptor).unwrap_err(),
        "task_attachment_unavailable"
    );
    let mut missing = descriptor;
    missing.read_owner = None;
    assert_eq!(
        ingest_staged_attachment(root.path(), &missing).unwrap_err(),
        "task_attachment_unavailable"
    );
}

#[test]
fn redacted_input_does_not_create_a_durable_reference_copy() {
    let root = tempfile::tempdir().unwrap();
    let (_, descriptor) = publish(
        root.path(),
        "redacted.key",
        b"synthetic rejected fixture bytes",
    );
    let result = ingest_staged_attachment(root.path(), &descriptor).unwrap();
    assert!(result.markdown.is_none());
    assert!(result.warning.is_some());
    assert_eq!(result.kind, "binary");
    assert!(!root.path().join("attachment_references").exists());
}

#[test]
fn model_reference_copies_are_private_unique_and_session_scoped() {
    use std::os::unix::fs::PermissionsExt as _;
    let first = tempfile::tempdir().unwrap();
    let second = tempfile::tempdir().unwrap();
    let (_, a) = publish(first.path(), "same.txt", b"first session");
    let (_, b) = publish(second.path(), "same.txt", b"second session");
    let ingested_a = ingest_staged_attachment(first.path(), &a).unwrap();
    let retry_a = ingest_staged_attachment(first.path(), &a).unwrap();
    let ingested_b = ingest_staged_attachment(second.path(), &b).unwrap();
    assert_ne!(ingested_a.path, retry_a.path);
    assert_ne!(ingested_a.path, ingested_b.path);
    for (result, root, bytes) in [
        (&ingested_a, first.path(), b"first session".as_slice()),
        (&ingested_b, second.path(), b"second session".as_slice()),
    ] {
        let path = std::path::Path::new(&result.path);
        assert!(path.starts_with(root));
        assert_eq!(
            std::fs::metadata(path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(
            std::fs::metadata(path.parent().unwrap())
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o700
        );
        assert_eq!(std::fs::read(path).unwrap(), bytes);
    }
    assert_eq!(
        ingest_staged_attachment(second.path(), &a).unwrap_err(),
        "task_attachment_invalid"
    );
}

#[test]
#[ignore = "requires local pandoc, pdftotext and 7z; no network or model"]
fn malformed_remote_documents_and_nested_archive_diagnostics_hide_private_sources() {
    use std::io::Write as _;
    let tools = crate::features::files::file_ingest::system_tools();
    assert!(tools.pandoc && tools.pdftotext && tools.sevenzip);
    let mut zip = zip::ZipWriter::new(std::io::Cursor::new(Vec::new()));
    zip.start_file("bad.pdf", zip::write::SimpleFileOptions::default())
        .unwrap();
    zip.write_all(b"not a PDF").unwrap();
    zip.start_file("notes.txt", zip::write::SimpleFileOptions::default())
        .unwrap();
    zip.write_all(b"User documentation: /proc/123/fd/45")
        .unwrap();
    let zip = zip.finish().unwrap().into_inner();
    for (name, bytes) in [
        ("bad.pdf", b"not a PDF".as_slice()),
        ("bad.docx", b"not a DOCX".as_slice()),
        ("archive.zip", zip.as_slice()),
    ] {
        let root = tempfile::tempdir().unwrap();
        let (owner, descriptor) = publish(root.path(), name, bytes);
        let result = ingest_staged_attachment(root.path(), &descriptor).unwrap();
        let diagnostic = if name.ends_with("zip") {
            let body = result.markdown.unwrap();
            assert!(body.contains("User documentation: /proc/123/fd/45"));
            body
        } else {
            assert!(result.markdown.is_none());
            result.warning.unwrap()
        };
        assert!(diagnostic.contains("attachment_conversion_failed"));
        assert!(!diagnostic.contains(owner.read_path().to_str().unwrap()));
        assert!(!diagnostic.contains(root.path().to_str().unwrap()));
        assert!(!diagnostic.contains("pinvou3-archive-"));
    }
}

#[test]
fn conversion_outputs_stay_private_with_permissive_umask_and_clean_up_on_error() {
    const PROBE: &str = "PINVOU_TEST_PRIVATE_CONVERSION_PROBE";
    if std::env::var(PROBE).as_deref() != Ok("1") {
        // Change the umask only in an isolated child, never in the test process.
        let output = std::process::Command::new("/bin/sh")
            .args(["-c", "umask 022; exec \"$@\"", "private-conversion-probe"])
            .arg(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "features::files::platform::linux_staged_attachment_tests::conversion_outputs_stay_private_with_permissive_umask_and_clean_up_on_error",
                "--nocapture",
            ])
            .env(PROBE, "1")
            .output()
            .unwrap();
        assert!(
            output.status.success()
                && String::from_utf8_lossy(&output.stdout).contains("1 passed;"),
            "private conversion probe failed: {} {}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
        return;
    }
    use std::os::unix::fs::PermissionsExt as _;
    for prefix in [
        "pinvou3-archive-",
        "pinvou3-msg-",
        "pinvou3-pdfocr-",
        "pinvou3-pdfpreview-",
        "pinvou3-libreoffice-",
    ] {
        let temporary = super::private_conversion_directory(prefix).unwrap();
        let path = temporary.path().to_owned();
        assert_eq!(
            std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o700,
            "converted bytes must not be accessible to another UID"
        );
        let other = super::private_conversion_directory(prefix).unwrap();
        assert_ne!(other.path(), path);
        let result = (move || -> std::io::Result<()> {
            std::fs::write(temporary.path().join("plaintext.txt"), b"synthetic content")?;
            Err(std::io::Error::other("synthetic parser failure"))
        })();
        assert!(result.is_err());
        assert!(!path.exists(), "parser errors must release converted bytes");
    }
}

#[test]
fn immutable_input_retains_format_scope_and_read_authority_without_serializing_it() {
    for (name, kind, body) in [
        ("report.txt", "text", b"verified text".as_slice()),
        ("data.json", "text", b"{\"verified\":true}".as_slice()),
        ("image.png", "image", b"image bytes".as_slice()),
        (
            "report.xlsx",
            "xlsx",
            include_bytes!("../../../../test-fixtures/multi_sheet.xlsx").as_slice(),
        ),
    ] {
        let root = tempfile::tempdir().unwrap();
        let folder = root.path().join("remote_uploads").join("ctr-up-linux");
        let owner = std::sync::Arc::new(
            crate::platform::filesystem::publish_private_read_path(
                crate::platform::filesystem::open_private_file_directory(&folder).unwrap(),
                std::ffi::OsStr::new(name),
                body,
            )
            .unwrap(),
        );
        owner.request_cleanup();
        let source = owner.read_path().to_owned();
        let descriptor = PublishedAttachment {
            handle_id: "ctr-up-linux".into(),
            path: owner.path().to_owned(),
            display_name: name.into(),
            mime_type: "application/octet-stream".into(),
            bytes: body.len() as u64,
            sha256: crate::platform::encoding::hex_lower(&Sha256::digest(body)),
            read_owner: Some(owner.clone()),
        };
        // A writable leaf at the logical name must never become a parser
        // input, even if it is introduced after upload authentication.
        std::fs::write(owner.path(), b"foreign content").unwrap();
        let ingested = ingest_staged_attachment(root.path(), &descriptor).unwrap();
        assert_eq!(ingested.kind, kind);
        assert_eq!(ingested.basename, name);
        if kind == "text" {
            assert_eq!(ingested.markdown.as_deref().unwrap().as_bytes(), body);
        }
        if kind == "xlsx" {
            let text = ingested.markdown.as_deref().unwrap();
            assert!(text.contains("Ultra 7 258V"));
            assert!(text.contains("83.6"));
        }
        let mut wire = serde_json::to_value(&ingested).unwrap();
        assert!(wire.get("read_owner").is_none());
        assert!(!wire.to_string().contains("/proc/"));
        wire["read_owner"] = serde_json::json!({"read_path": source.to_string_lossy()});
        let decoded: IngestResult = serde_json::from_value(wire).unwrap();
        assert!(decoded.read_owner.is_none());
        let mut renamed = descriptor.clone();
        renamed.display_name = "different.txt".into();
        assert!(ingest_staged_attachment(root.path(), &renamed).is_err());
        drop(renamed);
        let other_session = tempfile::tempdir().unwrap();
        assert!(ingest_staged_attachment(other_session.path(), &descriptor).is_err());
        drop(descriptor);
        drop(owner);
        assert_eq!(std::fs::read(ingested.read_path()).unwrap(), body);
        drop(ingested);
        assert!(!source.exists());
        assert_eq!(
            std::fs::read(folder.join(name)).unwrap(),
            b"foreign content"
        );
    }
}
