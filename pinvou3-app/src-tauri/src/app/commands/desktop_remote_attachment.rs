//! Internal upload lifecycle and host directory injection. A07 does not
//! expose transfer commands to a renderer: a real authenticated task consumer
//! must own bounded completed receipts before A08 can reopen an ingress.

use std::sync::Arc;

use crate::features::remote_control::controller::task::RemoteTaskService;
use crate::features::remote_control::controller::task::RemoteTaskTransferScope;
use crate::features::remote_control::controller::transfer::core::TransferRegistry;
use crate::features::remote_control::controller::transfer::staging::SessionIsolationStaging;
use crate::features::remote_control::controller::transfer::upload::AttachmentUploads;
use crate::features::sessions::SessionStore;

/// Managed state for the upload path (DEV-20 registers it).
pub(crate) struct DesktopRemoteAttachmentRuntime {
    uploads: Arc<AttachmentUploads>,
    tasks: Arc<RemoteTaskService>,
}

impl DesktopRemoteAttachmentRuntime {
    pub(crate) fn new(uploads: AttachmentUploads, tasks: Arc<RemoteTaskService>) -> Arc<Self> {
        Arc::new(Self {
            uploads: Arc::new(uploads),
            tasks,
        })
    }

    pub(crate) fn from_session_store(
        store: SessionStore,
        tasks: Arc<RemoteTaskService>,
        registry: Arc<TransferRegistry>,
    ) -> Arc<Self> {
        Self::new(
            AttachmentUploads::new(
                registry,
                Arc::new(SessionIsolationStaging::new(Arc::new(move |session_id| {
                    store
                        .session_roots(session_id)
                        .map(|roots| roots.ledger)
                        .map_err(|_| "task_session_not_found".to_owned())
                }))),
            ),
            tasks,
        )
    }

    pub(crate) fn uploads_cleanup_for_task(&self, task_id: &str) -> usize {
        self.uploads.cleanup_for_task(task_id)
    }

    pub(crate) fn cleanup_sink(
        &self,
    ) -> Arc<dyn crate::features::remote_control::controller::controlled::ControlledCleanupSink>
    {
        self.uploads.clone()
    }

    pub(crate) fn uploads_cleanup_for_connection(&self, connection_id: &str) -> usize {
        self.uploads.cleanup_for_connection(connection_id)
    }

    /// One lifecycle owner reaps the shared registry and retries staging
    /// cleanup even if peers stop sending commands. File I/O runs off Tokio's
    /// async workers; the weak owner lets application shutdown stop the loop.
    pub(crate) fn start_reaper(owner: &Arc<Self>) {
        // Setup may run on a plain host thread. Enter Tauri's Tokio runtime
        // before starting the one feature-owned interval.
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
    fn abort_transfers_for_task(&self, task_id: &str) -> usize {
        self.attachments.uploads_cleanup_for_task(task_id)
            + self.artifacts.downloads().cleanup_for_task(task_id)
    }
}

fn monotonic_millis() -> u64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    u64::try_from(ORIGIN.get_or_init(Instant::now).elapsed().as_millis()).unwrap_or(u64::MAX)
}
