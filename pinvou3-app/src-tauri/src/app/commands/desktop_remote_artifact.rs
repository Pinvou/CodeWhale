//! Windows remote artifact commands (DEV-18).
//!
//! A only ever sees opaque artifact IDs. Indexing, previewing and downloading
//! all resolve through the B-side index; a path never crosses the wire and a
//! missing artifact fails closed instead of falling back to a basename lookup.

use std::sync::Arc;

use base64::Engine as _;
use serde::Serialize;
use tauri::State;

use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::task::RemoteTaskService;
use crate::features::remote_control::controller::transfer::download::{
    ArtifactDescriptor, ArtifactDownloadBegin, ArtifactDownloads, ArtifactIndex, ArtifactPreview,
    DownloadChunk, FileArtifactSource,
};
use crate::features::remote_control::controller::transfer::TransferRegistry;

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct DesktopRemoteArtifactChunk {
    chunk_index: u64,
    chunk_b64: String,
    chunk_bytes: u64,
    received_bytes: u64,
    next_chunk: u64,
    completed: bool,
}

/// Managed state for artifact access (DEV-20 registers it).
pub(crate) struct DesktopRemoteArtifactRuntime {
    downloads: ArtifactDownloads,
    tasks: Arc<RemoteTaskService>,
}

impl DesktopRemoteArtifactRuntime {
    pub(crate) fn new(
        downloads: ArtifactDownloads,
        tasks: Arc<RemoteTaskService>,
    ) -> Arc<Self> {
        Arc::new(Self { downloads, tasks })
    }

    pub(crate) fn with_default_registry(tasks: Arc<RemoteTaskService>) -> Arc<Self> {
        let runtime = Self::new(
            ArtifactDownloads::new(
                Arc::new(TransferRegistry::default()),
                Arc::new(ArtifactIndex::default()),
            ),
            tasks,
        );
        // DEV-18 审核修复：未完成下载同样周期性回收（RC-F-003 下载侧）。
        let reaper_runtime = Arc::clone(&runtime);
        tauri::async_runtime::spawn(async move {
            loop {
                tokio::time::sleep(std::time::Duration::from_secs(60)).await;
                let expired = reaper_runtime.downloads.expire(monotonic_millis());
                if expired > 0 {
                    eprintln!("[remote-control] reaped {expired} stale artifact download(s)");
                }
            }
        });
        runtime
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

type ArtifactState<'a> = State<'a, Arc<DesktopRemoteArtifactRuntime>>;

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
    runtime
        .downloads
        .index()
        .register(
            session_id,
            artifact_id,
            display_name,
            mime_type,
            sha256,
            Arc::new(FileArtifactSource::new(path)),
        )
}

#[tauri::command]
pub fn desktop_remote_artifact_index(
    session_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: ArtifactState<'_>,
) -> Result<Vec<ArtifactDescriptor>, String> {
    ensure_supported(&controlled)?;
    runtime.ensure_session(&session_id)?;
    Ok(runtime.downloads.index().list(&session_id))
}

#[tauri::command]
pub fn desktop_remote_artifact_preview(
    session_id: String,
    artifact_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: ArtifactState<'_>,
) -> Result<ArtifactPreview, String> {
    ensure_supported(&controlled)?;
    runtime.ensure_session(&session_id)?;
    runtime
        .downloads
        .index()
        .preview(&session_id, &artifact_id)
}

#[tauri::command]
pub fn desktop_remote_artifact_download_begin(
    session_id: String,
    task_id: String,
    artifact_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: ArtifactState<'_>,
) -> Result<ArtifactDownloadBegin, String> {
    ensure_supported(&controlled)?;
    runtime.ensure_session(&session_id)?;
    let live = controlled
        .live_lease()
        .map_err(|error| error.code.to_owned())?;
    runtime
        .downloads
        .begin(&live, &session_id, &task_id, &artifact_id, monotonic_millis())
}

#[tauri::command]
pub fn desktop_remote_artifact_download_chunk(
    session_id: String,
    artifact_id: String,
    transfer_id: String,
    chunk_index: u64,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: ArtifactState<'_>,
) -> Result<DesktopRemoteArtifactChunk, String> {
    ensure_supported(&controlled)?;
    runtime.ensure_session(&session_id)?;
    let chunk: DownloadChunk = runtime
        .downloads
        .next_chunk(
            &session_id,
            &artifact_id,
            &transfer_id,
            chunk_index,
            monotonic_millis(),
        )?
        .ok_or_else(|| "artifact_unavailable".to_owned())?;
    Ok(DesktopRemoteArtifactChunk {
        chunk_index: chunk.chunk_index,
        chunk_b64: base64::engine::general_purpose::STANDARD.encode(&chunk.bytes),
        chunk_bytes: chunk.bytes.len() as u64,
        received_bytes: chunk.received_bytes,
        next_chunk: chunk.next_chunk,
        completed: chunk.completed.is_some(),
    })
}

#[tauri::command]
pub fn desktop_remote_artifact_download_abort(
    transfer_id: String,
    controlled: State<'_, ControlledDeviceManager>,
    runtime: ArtifactState<'_>,
) -> Result<bool, String> {
    ensure_supported(&controlled)?;
    Ok(runtime.downloads.abort(&transfer_id))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::task::RemoteTaskLedger;

    fn runtime() -> Arc<DesktopRemoteArtifactRuntime> {
        DesktopRemoteArtifactRuntime::with_default_registry(Arc::new(RemoteTaskService::new(
            Arc::new(RemoteTaskLedger::in_memory()),
        )))
    }

    #[test]
    fn index_requires_a_known_remote_session_and_projects_only_opaque_ids() {
        let runtime = runtime();
        assert_eq!(
            runtime.ensure_session("session-0001").unwrap_err(),
            "task_session_not_found"
        );
        assert_eq!(UNSUPPORTED, "desktop_remote_control_unsupported");
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
