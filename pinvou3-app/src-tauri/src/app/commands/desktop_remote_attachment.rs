//! Upload host composition: session root injection and shared lifecycle owner.
// architecture-guard: allow-target-cfg -- Windows-only regression verifies mandatory sharing leases during real file ingestion; production OS selection remains in platform::filesystem.
use crate::features::remote_control::controller::clock::monotonic_millis;
use crate::features::remote_control::controller::task::{
    RemoteTaskRecord, RemoteTaskService, RemoteTaskTransferScope,
};
use crate::features::remote_control::controller::transfer::core::TransferRegistry;
use crate::features::remote_control::controller::transfer::staging::SessionIsolationStaging;
use crate::features::remote_control::controller::transfer::upload::AttachmentUploads;
use crate::features::sessions::SessionStore;
use std::sync::Arc;

pub(crate) struct DesktopRemoteAttachmentRuntime {
    uploads: Arc<AttachmentUploads>,
}
impl DesktopRemoteAttachmentRuntime {
    pub(crate) fn new(uploads: AttachmentUploads) -> Arc<Self> {
        Arc::new(Self {
            uploads: Arc::new(uploads),
        })
    }
    pub(crate) fn from_session_store(
        store: SessionStore,
        _tasks: Arc<RemoteTaskService>,
        registry: Arc<TransferRegistry>,
    ) -> Arc<Self> {
        let stager = SessionIsolationStaging::new(Arc::new(move |id: &str| {
            store
                .session_roots(id)
                .map(|roots| roots.ledger)
                .map_err(|_| "task_session_not_found".to_owned())
        }));
        Self::new(AttachmentUploads::new(registry, Arc::new(stager)))
    }
    pub(crate) fn uploads(&self) -> &AttachmentUploads {
        &self.uploads
    }
    pub(crate) fn cleanup_sink(
        &self,
    ) -> Arc<dyn crate::features::remote_control::controller::controlled::ControlledCleanupSink>
    {
        self.uploads.clone()
    }
    pub(crate) fn uploads_cleanup_for_task(&self, id: &str) -> usize {
        self.uploads.cleanup_for_task(id)
    }
    pub(crate) fn uploads_cleanup_for_connection(&self, id: &str) -> usize {
        self.uploads.cleanup_for_connection(id)
    }
    pub(crate) fn start_reaper(owner: &Arc<Self>) {
        let uploads = owner.uploads.clone();
        tauri::async_runtime::spawn(async move {
            AttachmentUploads::start_reaper(&uploads, Arc::new(monotonic_millis));
        });
    }
}
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
    fn abort_transfers_for_task(&self, record: &RemoteTaskRecord) -> usize {
        let uploads = self
            .attachments
            .uploads()
            .cleanup_for_admission(record.sequence);
        let downloads = self
            .artifacts
            .downloads()
            .cleanup_for_admission(record.sequence);
        uploads + downloads
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reaper_uses_the_controller_clock_not_a_second_origin() {
        assert_eq!(
            monotonic_millis as fn() -> u64 as usize,
            crate::features::remote_control::controller::clock::monotonic_millis as fn() -> u64
                as usize
        );
        let before = crate::features::remote_control::controller::clock::monotonic_millis();
        let reaper_now = monotonic_millis();
        let after = crate::features::remote_control::controller::clock::monotonic_millis();
        assert!(before <= reaper_now && reaper_now <= after);
    }
}

#[cfg(all(test, windows))]
mod publication_ingest_tests {
    use super::*;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use crate::features::remote_control::controller::transfer::{
        TransferContext, TransferDirection, TransferMetadata,
    };
    use std::path::PathBuf;
    fn runtime(root: PathBuf) -> Arc<DesktopRemoteAttachmentRuntime> {
        DesktopRemoteAttachmentRuntime::new(AttachmentUploads::new(
            Arc::new(TransferRegistry::default()),
            Arc::new(SessionIsolationStaging::new(Arc::new(move |_| {
                Ok(root.clone())
            }))),
        ))
    }
    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
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
    fn metadata(data: &[u8]) -> TransferMetadata {
        use sha2::{Digest, Sha256};
        TransferMetadata {
            file_name: "report.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: data.len() as u64,
            sha256: crate::platform::encoding::hex_lower(&Sha256::digest(data)),
        }
    }
    const STAGING_DIR: &str = "remote_uploads";
    #[test]
    fn published_receipt_pins_the_bytes_and_path_until_terminal_cleanup() {
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
        let descriptors = runtime
            .uploads
            .consume_completed(
                std::slice::from_ref(&begin.handle_id),
                &lease(),
                "session-0001",
                "task-0001",
            )
            .unwrap();
        let path = &descriptors[0].path;
        assert!(std::fs::write(path, b"bad").is_err());
        assert!(std::fs::remove_file(path).is_err());
        assert!(std::fs::rename(path, path.with_extension("moved")).is_err());
        assert!(
            std::fs::rename(
                directory.path().join(STAGING_DIR),
                directory.path().join("replaced-root")
            )
            .is_err()
        );
        let ingested = crate::features::files::file_ingest::ingest_staged_attachment(
            directory.path(),
            &descriptors[0],
        )
        .unwrap();
        assert_eq!(ingested.markdown.as_deref(), Some("new"));
        runtime.uploads.cleanup_for_task("task-0001");
        assert_eq!(std::fs::read(path).unwrap(), b"new");
        drop(descriptors);
        runtime.uploads.expire(u64::MAX);
        assert!(
            !directory
                .path()
                .join(STAGING_DIR)
                .join(begin.handle_id)
                .exists()
        );
    }
}
