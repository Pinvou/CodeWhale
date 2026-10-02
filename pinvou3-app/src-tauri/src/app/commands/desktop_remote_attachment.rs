//! Windows remote attachment commands (DEV-17).
//!
//! A uploads into the B session isolation directory only. The command layer
//! never accepts a B path: the staging root is resolved from the session owner,
//! the file name is the bounded basename validated by the transfer core, and
//! writes stay anchored to private directory/file handles, never re-open a
//! caller-replaceable path. Cleanup failures remain explicit; restart sweeping
//! and the terminal lifecycle wiring are separate concerns.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use base64::Engine as _;
use parking_lot::Mutex;
use serde::Serialize;
use tauri::State;

use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::task::RemoteTaskService;
use crate::features::remote_control::controller::task::RemoteTaskTransferScope;
use crate::features::remote_control::controller::transfer::core::{
    CompletedTransfer, TransferContext, TransferDirection, TransferMetadata, TransferRegistry,
};
use crate::features::remote_control::controller::transfer::limits::MAX_TRANSFER_BYTES;
use crate::features::remote_control::controller::transfer::upload::{
    AttachmentUploads, UploadBegin, UploadProgress, UploadStaging,
};
use crate::features::sessions::SessionStore;
use crate::platform::filesystem::{PrivateFileDirectory, open_private_file_directory};

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
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
    total_bytes: u64,
}

impl SessionIsolationStaging {
    pub(crate) fn new(resolve_root: IsolationRootResolver) -> Self {
        Self {
            resolve_root,
            sessions: Mutex::new(HashMap::new()),
        }
    }

    /// Production resolver: the session's private ledger root.
    pub(crate) fn for_session_store(store: SessionStore) -> Self {
        Self::new(Arc::new(move |session_id: &str| {
            store
                .session_roots(session_id)
                .map(|roots| roots.ledger)
                .map_err(|_| "task_session_not_found".to_owned())
        }))
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

impl UploadStaging for SessionIsolationStaging {
    fn begin(
        &self,
        handle_id: &str,
        context: &TransferContext,
        metadata: &TransferMetadata,
    ) -> Result<(), String> {
        Self::validate_handle(handle_id)?;
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
        let root = (self.resolve_root)(&context.binding.session_id)?;
        let root =
            open_private_file_directory(&root).map_err(|_| "transfer_staging_failed".to_owned())?;
        let parent = root
            .create_private_child_directory(std::ffi::OsStr::new(STAGING_DIR))
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        let directory = parent
            .create_private_child_directory(std::ffi::OsStr::new(handle_id))
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        let file_name = safe_file_name(&metadata.file_name);
        let mut staging_name = format!(".pinvou-upload-{}", uuid::Uuid::new_v4().simple());
        if staging_name == file_name {
            staging_name.push_str("-part");
        }
        let file = match directory.create_delete_on_close_file(&staging_name) {
            Ok(file) => file,
            Err(_) => {
                drop(directory);
                let _ = parent.remove_empty_child_directory(std::ffi::OsStr::new(handle_id));
                return Err("transfer_staging_failed".to_owned());
            }
        };
        sessions.insert(
            handle_id.to_owned(),
            StagedUpload {
                parent,
                directory: Some(directory),
                file: Some(file),
                file_name,
                staging_name,
                total_bytes: metadata.total_bytes,
            },
        );
        Ok(())
    }

    fn accept_chunk(&self, handle_id: &str, _chunk_index: u64, bytes: &[u8]) -> Result<(), String> {
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

    fn commit(&self, transfer: &CompletedTransfer) -> Result<(), String> {
        use sha2::{Digest as _, Sha256};
        use std::io::{Read as _, Seek as _, Write as _};
        let mut sessions = self.sessions.lock();
        let entry = sessions
            .get_mut(&transfer.handle_id)
            .ok_or_else(|| "transfer_unknown".to_owned())?;
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
        entry
            .directory
            .as_ref()
            .ok_or_else(|| "transfer_unknown".to_owned())?
            .atomic_write_private_file(std::ffi::OsStr::new(&entry.file_name), &bytes)
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        sessions.remove(&transfer.handle_id);
        Ok(())
    }

    fn abort(&self, handle_id: &str) -> Result<(), String> {
        let mut sessions = self.sessions.lock();
        let Some(entry) = sessions.get_mut(handle_id) else {
            return Ok(());
        };
        entry.file.take();
        if let Some(directory) = entry.directory.as_ref() {
            for name in [entry.staging_name.as_str(), entry.file_name.as_str()] {
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

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct DesktopRemoteAttachmentBegin {
    transfer_id: String,
    chunk_bytes: usize,
    total_bytes: u64,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct DesktopRemoteAttachmentProgress {
    state: String,
    received_bytes: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    sha256: Option<String>,
}

/// Managed state for the upload path (DEV-20 registers it).
pub(crate) struct DesktopRemoteAttachmentRuntime {
    uploads: AttachmentUploads,
    tasks: Arc<RemoteTaskService>,
}

impl DesktopRemoteAttachmentRuntime {
    pub(crate) fn new(uploads: AttachmentUploads, tasks: Arc<RemoteTaskService>) -> Arc<Self> {
        Arc::new(Self { uploads, tasks })
    }

    pub(crate) fn from_session_store(
        store: SessionStore,
        tasks: Arc<RemoteTaskService>,
    ) -> Arc<Self> {
        Self::new(
            AttachmentUploads::new(
                Arc::new(TransferRegistry::default()),
                Arc::new(SessionIsolationStaging::for_session_store(store)),
            ),
            tasks,
        )
    }

    pub(crate) fn uploads_cleanup_for_task(&self, task_id: &str) -> usize {
        self.uploads.cleanup_for_task(task_id)
    }

    pub(crate) fn uploads_cleanup_for_connection(&self, connection_id: &str) -> usize {
        self.uploads.cleanup_for_connection(connection_id)
    }
}

type AttachmentState<'a> = State<'a, Arc<DesktopRemoteAttachmentRuntime>>;

/// Terminal cleanup for both transfer directions: the task owner calls this on
/// every terminal path so no half-finished attachment or artifact survives.
pub(crate) struct DesktopRemoteTransferScope {
    attachments: Arc<DesktopRemoteAttachmentRuntime>,
    artifacts: Arc<crate::app::commands::desktop_remote_artifact::DesktopRemoteArtifactRuntime>,
}

impl DesktopRemoteTransferScope {
    pub(crate) fn new(
        attachments: Arc<DesktopRemoteAttachmentRuntime>,
        artifacts: Arc<crate::app::commands::desktop_remote_artifact::DesktopRemoteArtifactRuntime>,
    ) -> Self {
        Self {
            attachments,
            artifacts,
        }
    }
}

impl RemoteTaskTransferScope for DesktopRemoteTransferScope {
    fn abort_transfers_for_task(&self, task_id: &str) -> usize {
        self.attachments.uploads_cleanup_for_task(task_id)
            + self.artifacts.downloads().cleanup_for_task(task_id)
    }
}

fn ensure_supported(controlled: &ControlledDeviceManager) -> Result<(), String> {
    if controlled.is_supported() {
        Ok(())
    } else {
        Err(UNSUPPORTED.to_owned())
    }
}

fn monotonic_millis() -> u64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

#[tauri::command]
#[allow(clippy::too_many_arguments)]
pub fn desktop_remote_attachment_begin(
    task_id: String,
    session_id: String,
    transfer_id: String,
    file_name: String,
    mime_type: String,
    total_bytes: u64,
    sha256: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: AttachmentState<'_>,
) -> Result<DesktopRemoteAttachmentBegin, String> {
    ensure_supported(&controlled)?;
    let live = controlled
        .live_lease()
        .map_err(|error| error.code.to_owned())?;
    if !runtime.tasks.is_remote_session(&session_id) {
        return Err("task_session_not_found".to_owned());
    }
    let context = TransferContext::new(&live, &session_id, &task_id, TransferDirection::Upload)
        .map_err(|error| error.code.to_owned())?;
    let metadata = TransferMetadata {
        file_name,
        mime_type,
        total_bytes,
        sha256,
    };
    let begin: UploadBegin = runtime
        .uploads
        .begin(context, metadata, monotonic_millis())?;
    let _ = transfer_id;
    Ok(DesktopRemoteAttachmentBegin {
        transfer_id: begin.handle_id,
        chunk_bytes: begin.chunk_bytes,
        total_bytes: begin.total_bytes,
    })
}

#[tauri::command]
pub fn desktop_remote_attachment_chunk(
    transfer_id: String,
    chunk_index: u64,
    chunk_b64: String,
    chunk_bytes: u64,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: AttachmentState<'_>,
) -> Result<DesktopRemoteAttachmentProgress, String> {
    ensure_supported(&controlled)?;
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(chunk_b64.as_bytes())
        .map_err(|_| "transfer_chunk_invalid".to_owned())?;
    if bytes.len() as u64 != chunk_bytes {
        return Err("transfer_chunk_invalid".to_owned());
    }
    let progress =
        runtime
            .uploads
            .accept_chunk(&transfer_id, chunk_index, &bytes, monotonic_millis())?;
    Ok(match progress {
        UploadProgress::Accepted { received_bytes, .. } => DesktopRemoteAttachmentProgress {
            state: "accepted".into(),
            received_bytes,
            sha256: None,
        },
        UploadProgress::Duplicate { received_bytes } => DesktopRemoteAttachmentProgress {
            state: "duplicate".into(),
            received_bytes,
            sha256: None,
        },
        UploadProgress::Completed { sha256, bytes } => DesktopRemoteAttachmentProgress {
            state: "completed".into(),
            received_bytes: bytes,
            sha256: Some(sha256),
        },
    })
}

#[tauri::command]
pub fn desktop_remote_attachment_abort(
    transfer_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: AttachmentState<'_>,
) -> Result<bool, String> {
    ensure_supported(&controlled)?;
    Ok(runtime.uploads.abort(&transfer_id))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use crate::features::remote_control::controller::task::RemoteTaskLedger;

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn runtime(root: PathBuf) -> Arc<DesktopRemoteAttachmentRuntime> {
        let stager = Arc::new(SessionIsolationStaging::new(Arc::new(
            move |_session_id: &str| Ok(root.clone()),
        )));
        DesktopRemoteAttachmentRuntime::new(
            AttachmentUploads::new(Arc::new(TransferRegistry::default()), stager),
            Arc::new(RemoteTaskService::new(Arc::new(
                RemoteTaskLedger::in_memory(),
            ))),
        )
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

    #[test]
    fn anonymous_staging_does_not_collide_with_part_bin_publication() {
        let directory = tempfile::tempdir().unwrap();
        let (stager, handle, context) = started_stager(directory.path(), b"part");
        stager.sessions.lock().get_mut(&handle).unwrap().file_name = "part.bin".into();
        stager.accept_chunk(&handle, 0, b"part").unwrap();
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
        stager.begin(&handle, &context, &metadata(bytes)).unwrap();
        (stager, handle, context)
    }

    fn completion(handle_id: String, context: TransferContext, bytes: &[u8]) -> CompletedTransfer {
        CompletedTransfer {
            handle_id,
            context,
            bytes: bytes.len() as u64,
            sha256: metadata(bytes).sha256,
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
            stager.begin(&handle, &context, &metadata(b"x")).unwrap();
            handles.push(handle);
        }
        let rejected = "ctr-up-capacity-rejected";
        assert_eq!(
            stager
                .begin(rejected, &context, &metadata(b"x"))
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
                .begin("ctr-up-linked-directory", &context, &metadata(b"x"))
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
        stager.accept_chunk(&handle, 0, b"new").unwrap();
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
        stager.accept_chunk(&handle, 0, b"new").unwrap();
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
        stager.accept_chunk(&handle, 0, b"new").unwrap();
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
        assert!(stager.accept_chunk(&handle, 0, b"new").is_err());
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
        stager.accept_chunk(&handle, 0, b"new").unwrap();
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
            stager.accept_chunk(&handle, 0, b"new").unwrap();
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
        stager.accept_chunk(&handle, 0, b"new").unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
        stager.commit(&completion(handle, context, b"new")).unwrap();
        assert_eq!(std::fs::read(&outside).unwrap(), b"untouched");
        assert_eq!(std::fs::read(&published).unwrap(), b"new");
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
    fn safe_file_name_never_escapes_the_staging_directory() {
        assert_eq!(safe_file_name("report.txt"), "report.txt");
        assert_eq!(safe_file_name("../../escape.txt"), "escape.txt");
        assert_eq!(safe_file_name(r"..\..\escape.txt"), "escape.txt");
        assert_eq!(safe_file_name(".."), "attachment.bin");
        assert_eq!(safe_file_name(""), "attachment.bin");
        assert!(safe_file_name(&"a".repeat(500)).len() <= 120);
    }
}
