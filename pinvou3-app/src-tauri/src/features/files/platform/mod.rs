pub(super) mod archive_entry;
mod model_reference;
mod office_stream;
pub(super) mod stale_attachment_cleanup;

pub(super) use model_reference::ModelReferenceScope;
pub(super) use office_stream::{
    convert as convert_office_stream, dependency as office_stream_dependency,
};

/// Converted attachment bytes need the same privacy as their sealed source.
/// Atomically create fresh scratch space; RAII also covers parser failures.
pub(super) fn private_conversion_directory(prefix: &str) -> std::io::Result<tempfile::TempDir> {
    let mut builder = tempfile::Builder::new();
    builder.prefix(prefix);
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        // TempDir's default is 0777 filtered by umask, not a private mode.
        // Set the mode at mkdir time, before a converter can write plaintext.
        builder.permissions(std::fs::Permissions::from_mode(0o700));
    }
    builder.tempdir()
}

#[cfg(all(test, target_os = "linux", target_pointer_width = "64"))]
mod linux_staged_attachment_tests;
