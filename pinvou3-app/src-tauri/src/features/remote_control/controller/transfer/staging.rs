//! Anchored private upload staging and protected completed-file publication.
// architecture-guard: allow-target-cfg -- Windows tests exercise mandatory sharing leases; runtime OS selection stays in platform::filesystem.
use super::core::{
    CompletedTransfer, TransferContext, TransferDirection, TransferHandle, TransferMetadata,
};
use super::limits::MAX_TRANSFER_BYTES;
#[cfg(test)]
use super::upload::AttachmentUploads;
use super::upload::UploadStaging;
use crate::platform::filesystem::{
    PrivateFileDirectory, PrivatePublicationCleanup, PublishedPrivateFile,
    open_private_file_directory, publish_private_read_path, supports_private_read_path_publication,
    take_private_publication_cleanup,
};
use parking_lot::Mutex;
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
const STAGING_DIR: &str = "remote_uploads";

/// Resolves the per-session isolation root. The production wiring uses the
/// session owner's private ledger directory; tests inject a temporary root.
pub(crate) type IsolationRootResolver = Arc<dyn Fn(&str) -> Result<PathBuf, String> + Send + Sync>;

pub(crate) struct SessionIsolationStaging {
    resolve_root: IsolationRootResolver,
    sessions: Mutex<HashMap<String, StagedUpload>>,
}

struct StagedUpload {
    parent: PrivateFileDirectory,
    directory: Option<PrivateFileDirectory>,
    file: Option<std::fs::File>,
    file_name: String,
    staging_name: String,
    directory_path: PathBuf,
    total_bytes: u64,
    published: Option<Arc<PublishedPrivateFile>>,
    rejected_publication: Option<PrivatePublicationCleanup>,
}

impl SessionIsolationStaging {
    pub(crate) fn new(resolve_root: IsolationRootResolver) -> Self {
        Self {
            resolve_root,
            sessions: Mutex::new(HashMap::new()),
        }
    }

    fn validate_handle(handle_id: &str) -> Result<(), String> {
        if !crate::features::remote_control::controller::transfer::core::is_controller_handle_id(
            handle_id,
        ) {
            return Err("transfer_unknown".to_owned());
        }
        // A handle id only contains ASCII alphanumerics and dashes, so it can
        // never escape the parent directory.
        if !handle_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        {
            return Err("transfer_unknown".to_owned());
        }
        Ok(())
    }
}

impl SessionIsolationStaging {
    fn begin_raw(
        &self,
        handle_id: &str,
        context: &TransferContext,
        metadata: &TransferMetadata,
    ) -> Result<(), String> {
        Self::validate_handle(handle_id)?;
        if !supports_private_read_path_publication() {
            return Err("unsupported".to_owned());
        }
        let mut sessions = self.sessions.lock();
        if sessions.contains_key(handle_id) {
            return Err("transfer_staging_failed".to_owned());
        }
        if sessions.len() >= MAX_STAGED_SESSIONS {
            return Err("transfer_capacity".to_owned());
        }
        if metadata.total_bytes == 0 || metadata.total_bytes > MAX_TRANSFER_BYTES {
            return Err("transfer_staging_failed".to_owned());
        }
        let root_path = (self.resolve_root)(&context.binding.session_id)?;
        if !root_path.is_absolute() {
            return Err("transfer_staging_failed".to_owned());
        }
        let root = open_private_file_directory(&root_path)
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        let parent = root
            .create_private_child_directory(std::ffi::OsStr::new(STAGING_DIR))
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        let directory = parent
            .create_private_child_directory(std::ffi::OsStr::new(handle_id))
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        let file_name = safe_file_name(&metadata.file_name);
        let directory_path = root_path.join(STAGING_DIR).join(handle_id);
        let mut staging_name = format!(".pinvou-upload-{}", uuid::Uuid::new_v4().simple());
        if staging_name == file_name {
            staging_name.push_str("-part");
        }
        // Reserve the exact directory authority before allocating its file.
        sessions.insert(
            handle_id.to_owned(),
            StagedUpload {
                parent,
                directory: Some(directory),
                file: None,
                file_name,
                staging_name,
                directory_path,
                total_bytes: metadata.total_bytes,
                published: None,
                rejected_publication: None,
            },
        );
        let entry = sessions.get_mut(handle_id).expect("target just reserved");
        entry.file = Some(
            entry
                .directory
                .as_ref()
                .expect("directory just reserved")
                .create_delete_on_close_file(&entry.staging_name)
                .map_err(|_| "transfer_staging_failed".to_owned())?,
        );
        Ok(())
    }

    fn accept_chunk_raw(
        &self,
        handle_id: &str,
        _chunk_index: u64,
        bytes: &[u8],
    ) -> Result<(), String> {
        use std::io::{Seek as _, Write as _};
        let mut sessions = self.sessions.lock();
        let entry = sessions
            .get_mut(handle_id)
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        let file = entry
            .file
            .as_mut()
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        let offset = file
            .stream_position()
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        if offset
            .checked_add(bytes.len() as u64)
            .is_none_or(|end| end > entry.total_bytes)
        {
            return Err("transfer_staging_failed".to_owned());
        }
        file.write_all(bytes)
            .map_err(|_| "transfer_staging_failed".to_owned())
    }

    fn commit(&self, transfer: &CompletedTransfer) -> Result<PathBuf, String> {
        use sha2::{Digest as _, Sha256};
        use std::io::{Read as _, Seek as _, Write as _};
        let mut sessions = self.sessions.lock();
        let entry = sessions
            .get_mut(&transfer.handle_id)
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        if entry.rejected_publication.is_some() {
            return Err("transfer_staging_failed".to_owned());
        }
        if transfer.bytes != entry.total_bytes || transfer.bytes > MAX_TRANSFER_BYTES {
            return Err("transfer_staging_failed".to_owned());
        }
        let file = entry
            .file
            .as_mut()
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        file.flush()
            .and_then(|()| file.rewind())
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        let mut bytes = Vec::new();
        file.take(entry.total_bytes + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        let digest: String = Sha256::digest(&bytes)
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect();
        if bytes.len() as u64 != entry.total_bytes || digest != transfer.sha256 {
            return Err("transfer_staging_failed".to_owned());
        }
        // A fresh anchor is acquired while the original chain is still pinned.
        let publication_directory = open_private_file_directory(&entry.directory_path)
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        let published = match publish_private_read_path(
            publication_directory,
            std::ffi::OsStr::new(&entry.file_name),
            &bytes,
        ) {
            Ok(published) => published,
            Err(mut error) => {
                entry.rejected_publication = take_private_publication_cleanup(&mut error);
                return Err("transfer_staging_failed".to_owned());
            }
        };
        let path = published.path().to_owned();
        entry.published = Some(Arc::new(published));
        // The anonymous part can close now, but published receipts stay
        // revocable through the same pinned directory until owner cleanup.
        entry.file.take();
        Ok(path)
    }

    fn published_read_owner(
        &self,
        handle_id: &str,
    ) -> Option<Arc<dyn crate::features::files::file_ingest::AttachmentReadOwner>> {
        self.sessions
            .lock()
            .get(handle_id)?
            .published
            .clone()
            .map(|owner| owner as Arc<dyn crate::features::files::file_ingest::AttachmentReadOwner>)
    }

    fn abort(&self, handle_id: &str) -> Result<(), String> {
        let mut sessions = self.sessions.lock();
        let Some(entry) = sessions.get_mut(handle_id) else {
            return Ok(());
        };
        if let Some(cleanup) = entry.rejected_publication.as_mut() {
            cleanup
                .cleanup()
                .map_err(|_| "transfer_staging_cleanup_failed".to_owned())?;
        }
        entry.rejected_publication.take();
        if entry
            .published
            .as_ref()
            .is_some_and(|owner| Arc::strong_count(owner) > 1)
        {
            return Err("transfer_staging_cleanup_failed".to_owned());
        }
        if let Some(owner) = entry.published.as_ref() {
            owner
                .cleanup()
                .map_err(|_| "transfer_staging_cleanup_failed".to_owned())?;
        }
        entry.published.take();
        entry.file.take();
        if let Some(directory) = entry.directory.as_ref() {
            // A failed publication never makes a replacement final leaf ours.
            for name in [entry.staging_name.as_str()] {
                directory
                    .remove_plain_file(std::ffi::OsStr::new(name))
                    .map_err(|_| "transfer_staging_cleanup_failed".to_owned())?;
            }
        }
        entry.directory.take();
        entry
            .parent
            .remove_empty_child_directory(std::ffi::OsStr::new(handle_id))
            .map_err(|_| "transfer_staging_cleanup_failed".to_owned())?;
        sessions.remove(handle_id);
        Ok(())
    }
}

const MAX_STAGED_SESSIONS: usize = 64;

/// Bounded basename. The transfer core already rejected separators, but the
/// staging layer re-derives the name from the final path component so a
/// platform-specific separator can never escape the isolation directory.
pub(crate) fn safe_file_name(file_name: &str) -> String {
    let candidate = Path::new(file_name)
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("attachment.bin");
    let cleaned: String = candidate
        .chars()
        .filter(|ch| !matches!(ch, '/' | '\\' | '\0') && !ch.is_control())
        .take(120)
        .collect();
    let cleaned = cleaned.trim_matches(['.', ' ']).to_owned();
    if cleaned.is_empty() {
        "attachment.bin".to_owned()
    } else {
        cleaned
    }
}

impl UploadStaging for SessionIsolationStaging {
    fn begin(
        &self,
        handle: &TransferHandle,
        context: &TransferContext,
        metadata: &TransferMetadata,
    ) -> Result<(), String> {
        if handle.direction() != TransferDirection::Upload
            || context.direction != TransferDirection::Upload
        {
            return Err("transfer_direction_mismatch".into());
        }
        self.begin_raw(handle.id(), context, metadata)
    }
    fn accept_chunk(
        &self,
        handle: &TransferHandle,
        index: u64,
        bytes: &[u8],
    ) -> Result<(), String> {
        if handle.direction() != TransferDirection::Upload {
            return Err("transfer_direction_mismatch".into());
        }
        self.accept_chunk_raw(handle.id(), index, bytes)
    }
    fn commit(&self, transfer: &CompletedTransfer) -> Result<PathBuf, String> {
        self.commit(transfer)
    }
    fn published_read_owner(
        &self,
        id: &str,
    ) -> Option<Arc<dyn crate::features::files::file_ingest::AttachmentReadOwner>> {
        self.published_read_owner(id)
    }
    fn abort(&self, id: &str) -> Result<(), String> {
        self.abort(id)
    }
}

#[cfg(all(test, windows))]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use crate::features::remote_control::controller::transfer::TransferDirection;

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    use super::super::core::TransferRegistry;
    struct TestUploadRuntime {
        uploads: AttachmentUploads,
    }
    impl TestUploadRuntime {
        fn new(uploads: AttachmentUploads) -> Arc<Self> {
            Arc::new(Self { uploads })
        }
    }

    fn runtime(root: PathBuf) -> Arc<TestUploadRuntime> {
        let stager = Arc::new(SessionIsolationStaging::new(Arc::new(
            move |_session_id: &str| Ok(root.clone()),
        )));
        TestUploadRuntime::new(AttachmentUploads::new(
            Arc::new(TransferRegistry::default()),
            stager,
        ))
    }

    fn metadata(bytes: &[u8]) -> TransferMetadata {
        use sha2::{Digest, Sha256};
        TransferMetadata {
            file_name: "report.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: bytes.len() as u64,
            sha256: Sha256::digest(bytes)
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect(),
        }
    }

    fn context() -> TransferContext {
        TransferContext::new(
            &lease(),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap()
    }

    #[test]
    fn anonymous_staging_does_not_collide_with_part_bin_publication() {
        let directory = tempfile::tempdir().unwrap();
        let (stager, handle, context) = started_stager(directory.path(), b"part");
        stager.sessions.lock().get_mut(&handle).unwrap().file_name = "part.bin".into();
        stager.accept_chunk_raw(&handle, 0, b"part").unwrap();
        stager
            .commit(&completion(handle.clone(), context, b"part"))
            .unwrap();
        assert_eq!(
            std::fs::read(
                directory
                    .path()
                    .join(STAGING_DIR)
                    .join(&handle)
                    .join("part.bin")
            )
            .unwrap(),
            b"part"
        );
        assert!(stager.sessions.lock().get(&handle).unwrap().file.is_none());
        stager.abort(&handle).unwrap();
        assert!(stager.sessions.lock().is_empty());
    }

    #[test]
    fn completed_cleanup_keeps_foreign_entries_and_can_retry() {
        let directory = tempfile::tempdir().unwrap();
        let (stager, handle, context) = started_stager(directory.path(), b"new");
        stager.accept_chunk_raw(&handle, 0, b"new").unwrap();
        let published = stager
            .commit(&completion(handle.clone(), context, b"new"))
            .unwrap();
        assert_eq!(std::fs::read(&published).unwrap(), b"new");
        assert!(stager.sessions.lock().get(&handle).unwrap().file.is_none());
        let foreign = published.parent().unwrap().join("foreign.txt");
        std::fs::write(&foreign, b"untouched").unwrap();
        assert_eq!(
            stager.abort(&handle).unwrap_err(),
            "transfer_staging_cleanup_failed"
        );
        assert!(!published.exists());
        assert_eq!(std::fs::read(&foreign).unwrap(), b"untouched");
        assert!(stager.sessions.lock().contains_key(&handle));
        std::fs::remove_file(&foreign).unwrap();
        stager.abort(&handle).unwrap();
        assert!(stager.sessions.lock().is_empty());
    }

    #[test]
    fn completed_owner_cleanup_releases_the_staging_capacity() {
        let directory = tempfile::tempdir().unwrap();
        let root = directory.path().to_path_buf();
        let stager = SessionIsolationStaging::new(Arc::new(move |_| Ok(root.clone())));
        let mut handles = Vec::new();
        for _ in 0..MAX_STAGED_SESSIONS {
            let handle = format!("ctr-up-{}", uuid::Uuid::new_v4().simple());
            let context = context();
            stager
                .begin_raw(&handle, &context, &metadata(b"x"))
                .unwrap();
            stager.accept_chunk_raw(&handle, 0, b"x").unwrap();
            stager
                .commit(&completion(handle.clone(), context, b"x"))
                .unwrap();
            handles.push(handle);
        }
        let next = format!("ctr-up-{}", uuid::Uuid::new_v4().simple());
        assert_eq!(
            stager
                .begin_raw(&next, &context(), &metadata(b"x"))
                .unwrap_err(),
            "transfer_capacity"
        );
        for handle in handles {
            stager.abort(&handle).unwrap();
        }
        stager
            .begin_raw(&next, &context(), &metadata(b"x"))
            .unwrap();
        stager.abort(&next).unwrap();
        assert!(stager.sessions.lock().is_empty());
    }

    fn started_stager(
        root: &Path,
        bytes: &[u8],
    ) -> (SessionIsolationStaging, String, TransferContext) {
        let root = root.to_path_buf();
        let stager = SessionIsolationStaging::new(Arc::new(move |_| Ok(root.clone())));
        let handle = format!("ctr-up-{}", uuid::Uuid::new_v4().simple());
        let context = TransferContext::new(
            &lease(),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        stager
            .begin_raw(&handle, &context, &metadata(bytes))
            .unwrap();
        (stager, handle, context)
    }

    fn completion(handle_id: String, context: TransferContext, bytes: &[u8]) -> CompletedTransfer {
        CompletedTransfer {
            handle_id,
            context,
            bytes: bytes.len() as u64,
            sha256: metadata(bytes).sha256,
            metadata: metadata(bytes),
        }
    }

    #[test]
    fn staging_capacity_is_reserved_before_filesystem_allocation() {
        let directory = tempfile::tempdir().unwrap();
        let root = directory.path().to_path_buf();
        let stager = SessionIsolationStaging::new(Arc::new(move |_| Ok(root.clone())));
        let context = TransferContext::new(
            &lease(),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        let mut handles = Vec::new();
        for _ in 0..MAX_STAGED_SESSIONS {
            let handle = format!("ctr-up-{}", uuid::Uuid::new_v4().simple());
            stager
                .begin_raw(&handle, &context, &metadata(b"x"))
                .unwrap();
            handles.push(handle);
        }
        let rejected = "ctr-up-capacity-rejected";
        assert_eq!(
            stager
                .begin_raw(rejected, &context, &metadata(b"x"))
                .unwrap_err(),
            "transfer_capacity"
        );
        assert!(!directory.path().join(STAGING_DIR).join(rejected).exists());
        for handle in handles {
            stager.abort(&handle).unwrap();
        }
        assert!(stager.sessions.lock().is_empty());
        assert_eq!(
            std::fs::read_dir(directory.path().join(STAGING_DIR))
                .unwrap()
                .count(),
            0
        );
    }

    #[test]
    fn staging_rejects_linked_handle_directory_without_touching_outside() {
        let directory = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        let parent = directory.path().join(STAGING_DIR);
        std::fs::create_dir(&parent).unwrap();
        let link = parent.join("ctr-up-linked-directory");
        if !crate::platform::filesystem::tests::try_link_dir(outside.path(), &link) {
            eprintln!("fixture unavailable: directory symlink/junction creation");
            return;
        }
        let root = directory.path().to_path_buf();
        let stager = SessionIsolationStaging::new(Arc::new(move |_| Ok(root.clone())));
        let context = TransferContext::new(
            &lease(),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        assert!(
            stager
                .begin_raw("ctr-up-linked-directory", &context, &metadata(b"x"))
                .is_err()
        );
        assert!(stager.sessions.lock().is_empty());
        assert_eq!(std::fs::read_dir(outside.path()).unwrap().count(), 0);
        crate::platform::filesystem::tests::remove_dir_link(&link);
    }

    #[test]
    fn replacing_anonymous_part_name_never_redirects_chunk_writes() {
        let directory = tempfile::tempdir().unwrap();
        let outside = directory.path().join("outside.txt");
        std::fs::write(&outside, b"untouched").unwrap();
        let (stager, handle, _) = started_stager(directory.path(), b"new");
        let name = stager
            .sessions
            .lock()
            .get(&handle)
            .unwrap()
            .staging_name
            .clone();
        let link = directory.path().join(STAGING_DIR).join(&handle).join(name);
        if !crate::platform::filesystem::tests::try_link_file(&outside, &link) {
            eprintln!("fixture unavailable or replacement denied: anonymous part file link");
            stager.abort(&handle).unwrap();
            assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
            return;
        }
        stager.accept_chunk_raw(&handle, 0, b"new").unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
        assert!(
            stager.abort(&handle).is_err(),
            "cleanup must reject, not follow an injected link"
        );
        assert!(stager.sessions.lock().contains_key(&handle));
        std::fs::remove_file(&link).unwrap();
        stager.abort(&handle).unwrap();
        assert!(stager.sessions.lock().is_empty());
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
    }

    #[test]
    fn atomic_publication_replaces_a_link_without_writing_its_target() {
        let directory = tempfile::tempdir().unwrap();
        let outside = directory.path().join("outside.txt");
        std::fs::write(&outside, b"untouched").unwrap();
        let (stager, handle, context) = started_stager(directory.path(), b"new");
        let published = directory
            .path()
            .join(STAGING_DIR)
            .join(&handle)
            .join("report.txt");
        if !crate::platform::filesystem::tests::try_link_file(&outside, &published) {
            eprintln!("fixture unavailable: publication file symlink creation");
            stager.abort(&handle).unwrap();
            return;
        }
        stager.accept_chunk_raw(&handle, 0, b"new").unwrap();
        stager.commit(&completion(handle, context, b"new")).unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
        assert_eq!(std::fs::read(&published).unwrap(), b"new");
        assert!(
            !std::fs::symlink_metadata(&published)
                .unwrap()
                .file_type()
                .is_symlink()
        );
    }

    #[test]
    fn pinned_parent_cannot_redirect_an_upload_to_a_replacement_path() {
        let directory = tempfile::tempdir().unwrap();
        let root = directory.path().join("session");
        let moved = directory.path().join("original-session");
        let outside = tempfile::tempdir().unwrap();
        let (stager, handle, context) = started_stager(&root, b"new");
        let renamed = std::fs::rename(&root, &moved).is_ok();
        if renamed && !crate::platform::filesystem::tests::try_link_dir(outside.path(), &root) {
            std::fs::rename(&moved, &root).unwrap();
            stager.abort(&handle).unwrap();
            eprintln!("fixture unavailable: replacement parent link creation");
            return;
        }
        stager.accept_chunk_raw(&handle, 0, b"new").unwrap();
        stager
            .commit(&completion(handle.clone(), context, b"new"))
            .unwrap();
        let original = if renamed { &moved } else { &root };
        assert_eq!(
            std::fs::read(original.join(STAGING_DIR).join(handle).join("report.txt")).unwrap(),
            b"new"
        );
        assert_eq!(std::fs::read_dir(outside.path()).unwrap().count(), 0);
        drop(stager);
        if renamed {
            crate::platform::filesystem::tests::remove_dir_link(&root);
            std::fs::rename(&moved, &root).unwrap();
        }
    }

    #[test]
    fn write_and_publication_failures_keep_cleanup_authority() {
        let directory = tempfile::tempdir().unwrap();
        let outside = directory.path().join("readonly-source.txt");
        std::fs::write(&outside, b"untouched").unwrap();
        let (stager, handle, _) = started_stager(directory.path(), b"new");
        stager.sessions.lock().get_mut(&handle).unwrap().file =
            Some(std::fs::File::open(&outside).unwrap());
        assert!(stager.accept_chunk_raw(&handle, 0, b"new").is_err());
        stager.abort(&handle).unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
        assert!(stager.sessions.lock().is_empty());
        let (stager, handle, context) = started_stager(directory.path(), b"new");
        let blocked = directory
            .path()
            .join(STAGING_DIR)
            .join(&handle)
            .join("report.txt");
        std::fs::create_dir(&blocked).unwrap();
        stager.accept_chunk_raw(&handle, 0, b"new").unwrap();
        assert!(
            stager
                .commit(&completion(handle.clone(), context, b"new"))
                .is_err()
        );
        assert!(stager.sessions.lock().get(&handle).unwrap().file.is_some());
        assert!(stager.abort(&handle).is_err());
        assert!(
            blocked.is_dir(),
            "cleanup must not recursively delete an unknown directory"
        );
        std::fs::remove_dir(&blocked).unwrap();
        stager.abort(&handle).unwrap();
        assert!(stager.sessions.lock().is_empty());
        assert!(!directory.path().join(STAGING_DIR).join(handle).exists());
    }

    #[test]
    fn commit_checks_length_and_digest_before_publication_and_abort_cleans_up() {
        let directory = tempfile::tempdir().unwrap();
        for wrong_length in [true, false] {
            let (stager, handle, context) = started_stager(directory.path(), b"new");
            stager.accept_chunk_raw(&handle, 0, b"new").unwrap();
            let mut transfer = completion(handle.clone(), context, b"new");
            if wrong_length {
                transfer.bytes += 1;
            } else {
                transfer.sha256 = metadata(b"bad").sha256;
            }
            assert!(stager.commit(&transfer).is_err());
            assert!(
                !directory
                    .path()
                    .join(STAGING_DIR)
                    .join(&handle)
                    .join("report.txt")
                    .exists()
            );
            assert!(stager.sessions.lock().get(&handle).unwrap().file.is_some());
            stager.abort(&handle).unwrap();
            assert!(stager.sessions.lock().is_empty());
            assert!(!directory.path().join(STAGING_DIR).join(handle).exists());
        }
    }

    #[test]
    fn hard_link_aliases_cannot_redirect_held_file_writes_or_atomic_publication() {
        let directory = tempfile::tempdir().unwrap();
        let outside = directory.path().join("outside.txt");
        std::fs::write(&outside, b"untouched").unwrap();
        let (stager, handle, context) = started_stager(directory.path(), b"new");
        let name = stager
            .sessions
            .lock()
            .get(&handle)
            .unwrap()
            .staging_name
            .clone();
        let parent = directory.path().join(STAGING_DIR).join(&handle);
        let part_alias = parent.join(name);
        match std::fs::hard_link(&outside, &part_alias) {
            Ok(()) => {}
            Err(error) => assert!(
                matches!(
                    error.kind(),
                    std::io::ErrorKind::AlreadyExists | std::io::ErrorKind::PermissionDenied
                ),
                "the OS must explicitly deny replacing the live delete-pending file: {error}"
            ),
        }
        let published = parent.join("report.txt");
        std::fs::hard_link(&outside, &published).unwrap();
        stager.accept_chunk_raw(&handle, 0, b"new").unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
        stager
            .commit(&completion(handle.clone(), context, b"new"))
            .unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
        assert_eq!(std::fs::read(&published).unwrap(), b"new");
        stager.abort(&handle).unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
        assert!(stager.sessions.lock().is_empty());
    }

    #[test]
    fn staging_writes_inside_the_session_isolation_directory_only() {
        let directory = tempfile::tempdir().unwrap();
        let runtime = runtime(directory.path().to_path_buf());
        let data = b"hello attachment".to_vec();
        let context = TransferContext::new(
            &lease(),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        let begin = runtime
            .uploads
            .begin(context, metadata(&data), 1_000)
            .unwrap();
        runtime
            .uploads
            .accept_chunk(&begin.handle_id, 0, &data, 1_100)
            .unwrap();
        let published = directory
            .path()
            .join(STAGING_DIR)
            .join(&begin.handle_id)
            .join("report.txt");
        assert!(published.exists());
        assert_eq!(std::fs::read(&published).unwrap(), data);
        // Nothing was written outside the isolation directory.
        assert!(!directory.path().join("report.txt").exists());
    }

    #[test]
    fn abort_removes_the_staging_directory_and_is_idempotent() {
        let directory = tempfile::tempdir().unwrap();
        let runtime = runtime(directory.path().to_path_buf());
        let data = b"partial".to_vec();
        let context = TransferContext::new(
            &lease(),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        let begin = runtime
            .uploads
            .begin(context, metadata(&data), 1_000)
            .unwrap();
        runtime
            .uploads
            .accept_chunk(&begin.handle_id, 0, &data[..4], 1_100)
            .unwrap();
        let staging = directory.path().join(STAGING_DIR).join(&begin.handle_id);
        assert!(staging.exists());
        assert!(runtime.uploads.abort(&begin.handle_id));
        assert!(!staging.exists());
        assert!(!runtime.uploads.abort(&begin.handle_id));
    }

    #[test]
    fn completed_receipts_cleanup_without_deadlocking_their_own_read_owner() {
        for consumed in [false, true] {
            let directory = tempfile::tempdir().unwrap();
            let runtime = runtime(directory.path().to_owned());
            let begin = runtime
                .uploads
                .begin(context(), metadata(b"new"), 1)
                .unwrap();
            runtime
                .uploads
                .accept_chunk(&begin.handle_id, 0, b"new", 2)
                .unwrap();
            let path = directory
                .path()
                .join(STAGING_DIR)
                .join(&begin.handle_id)
                .join("report.txt");
            let descriptors = if consumed {
                runtime
                    .uploads
                    .consume_completed(
                        std::slice::from_ref(&begin.handle_id),
                        &lease(),
                        "session-0001",
                        "task-0001",
                    )
                    .unwrap()
            } else {
                Vec::new()
            };
            if consumed {
                assert_eq!(runtime.uploads.cleanup_for_task("task-0001"), 1);
                assert!(path.exists(), "an executing reader must retain the object");
                assert_eq!(std::fs::read(&path).unwrap(), b"new");
                assert_eq!(
                    runtime
                        .uploads
                        .require_upload_binding(
                            &begin.handle_id,
                            &lease(),
                            "session-0001",
                            "task-0001"
                        )
                        .unwrap_err(),
                    "transfer_unknown"
                );
                drop(descriptors);
                runtime.uploads.expire(u64::MAX);
            } else {
                assert_eq!(runtime.uploads.expire(u64::MAX), 1);
            }
            assert!(!path.exists());
            assert!(!path.parent().unwrap().exists());
        }
    }

    #[test]
    fn rejected_publication_retains_cleanup_identity_for_reaper_and_releases_capacity() {
        use std::os::windows::fs::OpenOptionsExt as _;
        use windows_sys::Win32::Storage::FileSystem::{FILE_GENERIC_WRITE, FILE_SHARE_READ};
        for replace in [false, true] {
            let directory = tempfile::tempdir().unwrap();
            let root = directory.path().to_owned();
            let stager = Arc::new(SessionIsolationStaging::new(Arc::new(move |_| {
                Ok(root.clone())
            })));
            let uploads =
                AttachmentUploads::new(Arc::new(TransferRegistry::default()), stager.clone());
            let begin = uploads.begin(context(), metadata(b"new"), 1).unwrap();
            let blocker = Arc::new(Mutex::new(None));
            let captured_blocker = blocker.clone();
            crate::platform::filesystem::PRIVATE_PUBLICATION_BEFORE_READ.with(|hook| {
                *hook.borrow_mut() = Some(Box::new(move |path| {
                    *captured_blocker.lock() = Some(
                        std::fs::OpenOptions::new()
                            .access_mode(FILE_GENERIC_WRITE)
                            .share_mode(FILE_SHARE_READ)
                            .open(path)
                            .unwrap(),
                    );
                }));
            });
            assert_eq!(
                uploads
                    .accept_chunk(&begin.handle_id, 0, b"new", 2)
                    .unwrap_err(),
                "transfer_staging_failed"
            );
            let path = directory
                .path()
                .join(STAGING_DIR)
                .join(&begin.handle_id)
                .join("report.txt");
            assert!(path.exists());
            {
                let entries = stager.sessions.lock();
                let entry = entries.get(&begin.handle_id).unwrap();
                assert!(entry.rejected_publication.is_some());
                assert!(
                    entry.published.is_none(),
                    "rejected content has no read owner"
                );
            }
            uploads.expire(u64::MAX);
            assert!(stager.sessions.lock().contains_key(&begin.handle_id));
            blocker.lock().take();
            if replace {
                let saved = path.with_extension("original");
                std::fs::rename(&path, &saved).unwrap();
                std::fs::write(&path, b"foreign").unwrap();
                uploads.expire(u64::MAX);
                assert_eq!(std::fs::read(&path).unwrap(), b"foreign");
                assert_eq!(std::fs::read(&saved).unwrap(), b"new");
                assert!(
                    stager
                        .sessions
                        .lock()
                        .get(&begin.handle_id)
                        .unwrap()
                        .rejected_publication
                        .is_some()
                );
                // Fixture restores the original object; the reaper never deletes the replacement.
                std::fs::remove_file(&path).unwrap();
                std::fs::rename(saved, &path).unwrap();
            }
            uploads.expire(u64::MAX);
            assert!(!path.parent().unwrap().exists());
            assert!(stager.sessions.lock().is_empty());
            // All 64 slots, not just a task's protocol budget, are reusable.
            let mut handles = Vec::new();
            for _ in 0..MAX_STAGED_SESSIONS {
                let handle = format!("ctr-up-{}", uuid::Uuid::new_v4().simple());
                stager
                    .begin_raw(&handle, &context(), &metadata(b"x"))
                    .unwrap();
                handles.push(handle);
            }
            for handle in handles {
                stager.abort(&handle).unwrap();
            }
        }
    }
}

#[cfg(test)]
mod portable_staging_tests {
    use super::*;
    #[test]
    fn safe_file_name_never_escapes_the_staging_directory() {
        assert_eq!(safe_file_name("report.txt"), "report.txt");
        assert_eq!(safe_file_name("../../escape.txt"), "escape.txt");
        assert_eq!(safe_file_name(r"..\..\escape.txt"), "escape.txt");
        assert_eq!(safe_file_name(".."), "attachment.bin");
        assert_eq!(safe_file_name(""), "attachment.bin");
        assert!(safe_file_name(&"a".repeat(500)).len() <= 120);
    }

    #[test]
    fn unsupported_read_path_publication_is_rejected_before_root_resolution() {
        if supports_private_read_path_publication() {
            return;
        }
        use crate::features::remote_control::controller::protocol::LeaseContext;
        use crate::features::remote_control::controller::transfer::TransferDirection;
        let stager = SessionIsolationStaging::new(Arc::new(|_| {
            panic!("must not allocate filesystem resources")
        }));
        let lease = LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        };
        let context = TransferContext::new(
            &lease,
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap();
        let metadata = TransferMetadata {
            file_name: "report.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: 1,
            sha256: "0".repeat(64),
        };
        assert_eq!(
            stager
                .begin_raw("ctr-up-unsupported", &context, &metadata)
                .unwrap_err(),
            "unsupported"
        );
        assert!(stager.sessions.lock().is_empty());
    }
}
