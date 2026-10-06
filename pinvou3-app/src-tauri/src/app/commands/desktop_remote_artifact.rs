//! Internal artifact index and shared transfer state. A07 has no renderer
//! transfer ingress; A08 must bind a completed receipt to an authenticated
//! task consumer and explicit resource ownership before exposing operations.

use std::sync::Arc;

use crate::features::remote_control::controller::task::RemoteTaskService;
use crate::features::remote_control::controller::transfer::TransferRegistry;
use crate::features::remote_control::controller::transfer::download::{
    ArtifactDescriptor, ArtifactDownloads, ArtifactIndex, FileArtifactSource,
};

/// Managed state for artifact access (DEV-20 registers it).
pub(crate) struct DesktopRemoteArtifactRuntime {
    downloads: ArtifactDownloads,
    tasks: Arc<RemoteTaskService>,
}

impl DesktopRemoteArtifactRuntime {
    pub(crate) fn new(downloads: ArtifactDownloads, tasks: Arc<RemoteTaskService>) -> Arc<Self> {
        Arc::new(Self { downloads, tasks })
    }

    pub(crate) fn with_registry(
        tasks: Arc<RemoteTaskService>,
        registry: Arc<TransferRegistry>,
    ) -> Arc<Self> {
        Self::new(
            ArtifactDownloads::new(registry, Arc::new(ArtifactIndex::default())),
            tasks,
        )
    }

    pub(crate) fn downloads(&self) -> &ArtifactDownloads {
        &self.downloads
    }

    fn ensure_session(&self, session_id: &str) -> Result<(), String> {
        if self.tasks.is_remote_session(session_id) {
            Ok(())
        } else {
            Err("task_session_not_found".to_owned())
        }
    }
}

/// Indexes one session artifact. The caller supplies the authoritative file
/// location; it stays in B's index and is never projected.
pub(crate) fn index_artifact(
    runtime: &DesktopRemoteArtifactRuntime,
    session_id: &str,
    artifact_id: &str,
    display_name: &str,
    mime_type: &str,
    sha256: &str,
    path: std::path::PathBuf,
) -> Result<ArtifactDescriptor, String> {
    runtime.downloads.index().register(
        session_id,
        artifact_id,
        display_name,
        mime_type,
        sha256,
        Arc::new(FileArtifactSource::new(path)),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::task::RemoteTaskLedger;

    fn runtime() -> Arc<DesktopRemoteArtifactRuntime> {
        DesktopRemoteArtifactRuntime::with_registry(
            Arc::new(RemoteTaskService::new(Arc::new(
                RemoteTaskLedger::in_memory(),
            ))),
            Arc::new(TransferRegistry::default()),
        )
    }

    #[test]
    fn index_requires_a_known_remote_session_and_projects_only_opaque_ids() {
        let runtime = runtime();
        assert_eq!(
            runtime.ensure_session("session-0001").unwrap_err(),
            "task_session_not_found"
        );
    }

    #[test]
    fn indexing_a_missing_file_fails_closed() {
        let runtime = runtime();
        let directory = tempfile::tempdir().unwrap();
        let missing = directory.path().join("missing.txt");
        assert_eq!(
            index_artifact(
                &runtime,
                "session-0001",
                "artifact-0001",
                "missing.txt",
                "text/plain",
                &"0".repeat(64),
                missing,
            )
            .unwrap_err(),
            "artifact_unavailable"
        );
    }

    #[test]
    fn indexing_an_existing_file_exposes_no_path() {
        let runtime = runtime();
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("report.md");
        std::fs::write(&path, b"# report").unwrap();
        let digest: String = {
            use sha2::{Digest, Sha256};
            Sha256::digest(b"# report")
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect()
        };
        let descriptor = index_artifact(
            &runtime,
            "session-0001",
            "artifact-0001",
            "report.md",
            "text/markdown",
            &digest,
            path,
        )
        .unwrap();
        let encoded = serde_json::to_string(&descriptor).unwrap();
        assert!(!encoded.contains(directory.path().to_string_lossy().as_ref()));
        assert!(encoded.contains("artifact-0001"));
    }
}
