//! Windows remote attachment commands (DEV-17).
//!
//! A uploads into the B session isolation directory only. The command layer
//! never accepts a B path: the staging root is resolved from the session owner,
//! the file name is the bounded basename validated by the transfer core, and
//! every failure path drops the staging directory so no orphan cache survives.

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
use crate::features::remote_control::controller::transfer::upload::{
    AttachmentUploads, UploadBegin, UploadProgress, UploadStaging,
};
use crate::features::sessions::SessionStore;

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
const STAGING_DIR: &str = "remote_uploads";
const STAGING_PART: &str = "part.bin";

/// Resolves the per-session isolation root. The production wiring uses the
/// session owner's private ledger directory; tests inject a temporary root.
pub(crate) type IsolationRootResolver = Arc<dyn Fn(&str) -> Result<PathBuf, String> + Send + Sync>;

pub(crate) struct SessionIsolationStaging {
    resolve_root: IsolationRootResolver,
    /// handle_id -> (session_id, published file name).
    sessions: Mutex<HashMap<String, (String, String)>>,
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

    fn staging_dir(&self, handle_id: &str) -> Result<PathBuf, String> {
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
        let root = self.roots_for_handle(handle_id)?;
        Ok(root.join(STAGING_DIR).join(handle_id))
    }

    fn roots_for_handle(&self, handle_id: &str) -> Result<PathBuf, String> {
        let session_id = self.session_for_handle(handle_id)?;
        (self.resolve_root)(&session_id)
    }

    fn session_for_handle(&self, handle_id: &str) -> Result<String, String> {
        self.sessions
            .lock()
            .get(handle_id)
            .map(|(session_id, _)| session_id.clone())
            .ok_or_else(|| "transfer_unknown".to_owned())
    }
}

impl UploadStaging for SessionIsolationStaging {
    fn begin(
        &self,
        handle_id: &str,
        context: &TransferContext,
        metadata: &TransferMetadata,
    ) -> Result<(), String> {
        let root = (self.resolve_root)(&context.binding.session_id)?;
        let directory = root.join(STAGING_DIR).join(handle_id);
        std::fs::create_dir_all(&directory).map_err(|_| "transfer_staging_failed".to_owned())?;
        let target = directory.join(STAGING_PART);
        std::fs::File::create(&target).map_err(|_| "transfer_staging_failed".to_owned())?;
        let mut sessions = self.sessions.lock();
        if sessions.len() >= MAX_STAGED_SESSIONS && !sessions.contains_key(handle_id) {
            return Err("transfer_capacity".to_owned());
        }
        sessions.insert(
            handle_id.to_owned(),
            (
                context.binding.session_id.clone(),
                metadata.file_name.clone(),
            ),
        );
        Ok(())
    }

    fn accept_chunk(&self, handle_id: &str, _chunk_index: u64, bytes: &[u8]) -> Result<(), String> {
        use std::io::Write as _;
        let directory = self.staging_dir(handle_id)?;
        let target = directory.join(STAGING_PART);
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .open(&target)
            .map_err(|_| "transfer_staging_failed".to_owned())?;
        file.write_all(bytes)
            .map_err(|_| "transfer_staging_failed".to_owned())
    }

    fn commit(&self, transfer: &CompletedTransfer) -> Result<(), String> {
        let directory = self.staging_dir(&transfer.handle_id)?;
        let (_, file_name) = self
            .sessions
            .lock()
            .get(&transfer.handle_id)
            .cloned()
            .ok_or_else(|| "transfer_unknown".to_owned())?;
        let staged = directory.join(STAGING_PART);
        let published = directory.join(safe_file_name(&file_name));
        std::fs::rename(&staged, &published).map_err(|_| "transfer_staging_failed".to_owned())?;
        self.sessions.lock().remove(&transfer.handle_id);
        Ok(())
    }

    fn abort(&self, handle_id: &str) -> Result<(), String> {
        let directory = match self.staging_dir(handle_id) {
            Ok(directory) => directory,
            Err(_) => return Ok(()),
        };
        self.sessions.lock().remove(handle_id);
        match std::fs::remove_dir_all(&directory) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(_) => Err("transfer_staging_cleanup_failed".to_owned()),
        }
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
