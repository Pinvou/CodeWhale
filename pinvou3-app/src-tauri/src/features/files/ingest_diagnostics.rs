//! Remote ingestion may expose warnings through prompts and nested archives.
//! Keep user document text intact; suppress only converter/IO diagnostics.
use std::cell::Cell;

pub(in crate::features::files) const CONVERSION_FAILED: &str = "attachment_conversion_failed";
pub(in crate::features::files) const OFFICE_STREAM_MISSING: &str =
    "attachment_office_stream_dependency_missing";

thread_local! {
    static PRIVATE: Cell<bool> = const { Cell::new(false) };
}

pub(super) fn with_private<R>(action: impl FnOnce() -> R) -> R {
    struct Reset(bool);
    impl Drop for Reset {
        fn drop(&mut self) {
            PRIVATE.set(self.0);
        }
    }
    let _reset = Reset(PRIVATE.replace(true));
    action()
}

pub(super) fn safe_error(detail: impl Into<String>) -> String {
    let detail = detail.into();
    if PRIVATE.get() && detail != OFFICE_STREAM_MISSING {
        CONVERSION_FAILED.to_owned()
    } else {
        detail
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn private_diagnostics_are_scoped_and_unwind_safe_without_changing_document_text() {
        let detail = "converter failed /proc/123/fd/45 /tmp/pinvou-private/document";
        assert_eq!(safe_error(detail), detail);
        with_private(|| {
            assert_eq!(safe_error(detail), CONVERSION_FAILED);
            assert_eq!(safe_error(OFFICE_STREAM_MISSING), OFFICE_STREAM_MISSING);
            let user_body = format!("User documentation: {detail}");
            let result = super::super::IngestResult::with_markdown(
                "text",
                "report.txt",
                std::path::Path::new("report.txt"),
                user_body.len() as u64,
                user_body.clone(),
            );
            assert_eq!(result.markdown.as_deref(), Some(user_body.as_str()));
            let _ =
                std::panic::catch_unwind(|| with_private(|| panic!("synthetic parser failure")));
            assert_eq!(safe_error(detail), CONVERSION_FAILED);
        });
        assert_eq!(safe_error(detail), detail);
    }
}
