//! Process-private attachment publication authority and descriptor.
use std::path::{Path, PathBuf};

/// A narrow, process-only publication authority. Production uses the OS lease;
/// deterministic transfer tests can inject an owner without filesystem effects.
pub(crate) trait AttachmentReadOwner: std::fmt::Debug + Send + Sync {
    fn path(&self) -> &Path;
    fn read_bounded(&self, maximum_bytes: u64) -> std::io::Result<Vec<u8>>;
    fn request_cleanup(&self);
}

impl PartialEq for dyn AttachmentReadOwner {
    fn eq(&self, other: &Self) -> bool {
        std::ptr::eq(self, other)
    }
}
impl Eq for dyn AttachmentReadOwner {}

impl AttachmentReadOwner for crate::platform::filesystem::PublishedPrivateFile {
    fn path(&self) -> &Path {
        self.path()
    }
    fn read_bounded(&self, maximum_bytes: u64) -> std::io::Result<Vec<u8>> {
        self.read_bounded(maximum_bytes)
    }
    fn request_cleanup(&self) {
        self.request_cleanup();
    }
}

/// B-local managed attachment descriptor. The path is process-private and
/// must never be serialized into task payloads, journals, or controller wire.
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct PublishedAttachment {
    pub(crate) handle_id: String,
    pub(crate) path: PathBuf,
    pub(crate) display_name: String,
    pub(crate) mime_type: String,
    pub(crate) bytes: u64,
    pub(crate) sha256: String,
    /// Process-only authority; never serialized or copied into a journal.
    pub(crate) read_owner: Option<std::sync::Arc<dyn AttachmentReadOwner>>,
}
