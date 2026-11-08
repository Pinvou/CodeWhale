// Test host: production shared core and real text decoder; untested formats fail closed.
pub(crate) use crate::file_limits::MAX_FILE_BYTES;
pub(crate) use crate::published_attachment::*;
pub const ATTACHMENT_FILE_TOO_LARGE: &str = "attachment_file_too_large";
pub const ATTACHMENT_ARCHIVE_TOO_MANY_ENTRIES: &str = "attachment_archive_too_many_entries";
pub const ATTACHMENT_ARCHIVE_EXPANDED_TOO_LARGE: &str = "attachment_archive_expanded_too_large";
pub const ATTACHMENT_ARCHIVE_UNSAFE_ENTRY: &str = "attachment_archive_unsafe_entry";
#[path = "../../../src/features/files/ingest_diagnostics.rs"]
mod ingest_diagnostics;
#[path = "../../../src/features/files/platform/model_reference.rs"]
mod model_reference;
use model_reference::ModelReferenceScope;
#[path = "../../../src/features/files/ingest_core.rs"]
mod ingest_core;
#[path = "../../../src/features/files/text_decode.rs"]
mod text_decode;
pub(crate) use ingest_core::{IngestResult, ingest_staged_attachment};
use ingest_core::{binary_placeholder, estimate_tokens, redacted_secret_placeholder};
use std::path::Path;
mod ingest_pdf {
    use super::*;
    pub(super) fn ingest_pdf(_: &Path, _: String, _: String, _: u64) -> IngestResult {
        panic!("non-text harness branch")
    }
}
mod visual_preview {
    use super::*;
    pub(super) fn ingest_image(_: &Path, _: String, _: String, _: u64) -> IngestResult {
        panic!("non-text harness branch")
    }
}
mod ingest_archive {
    use super::*;
    pub(super) fn ingest_archive(
        _: &Path,
        _: String,
        _: String,
        _: u64,
    ) -> Result<IngestResult, ingest_core::AttachmentLimitViolation> {
        panic!("non-text harness branch")
    }
}
mod ingest_email {
    use super::*;
    pub(super) fn ingest_email(_: &Path, _: String, _: String, _: u64, _: &str) -> IngestResult {
        panic!("non-text harness branch")
    }
}
mod ingest_office {
    use super::*;
    pub(super) fn ingest_pandoc(_: &Path, _: String, _: u64, _: &str) -> IngestResult {
        panic!("non-text harness branch")
    }
    pub(super) fn ingest_office_text(_: &Path, _: String, _: u64, _: &str) -> IngestResult {
        panic!("non-text harness branch")
    }
    pub(super) fn ingest_presentation(_: &Path, _: String, _: u64, _: &str) -> IngestResult {
        panic!("non-text harness branch")
    }
    pub(super) fn ingest_spreadsheet(
        _: &Path,
        _: String,
        _: String,
        _: u64,
        _: &str,
    ) -> IngestResult {
        panic!("non-text harness branch")
    }
}
