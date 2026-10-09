//! Windows remote artifact commands (DEV-18).
//!
//! A only ever sees opaque artifact IDs. Indexing, previewing and downloading
//! all resolve through the B-side index; a path never crosses the wire and a
//! missing artifact fails closed instead of falling back to a basename lookup.
// architecture-guard: allow-target-cfg -- cross-platform regression test creates an OS-native file symlink to prove project artifact capture never follows it; production capture remains platform-neutral.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{
    Arc,
    atomic::{AtomicU64, Ordering},
};

use parking_lot::Mutex;
use sha2::{Digest, Sha256};
use walkdir::WalkDir;

use crate::features::codex_acp::remote_turn::{RemoteCodeArtifactPort, RemoteCodeCaptureToken};
#[cfg(test)]
use crate::features::remote_control::controller::journal::JournalKind;
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::task::{
    ControllerTaskEventSink, RemoteTaskArtifactScope, RemoteTaskRecord, RemoteTaskService,
    RemoteTaskState, TASK_MODE_CODE, TaskLeaseBinding,
};
use crate::features::remote_control::controller::transfer::TransferRegistry;
use crate::features::remote_control::controller::transfer::download::{
    ArtifactDescriptor, ArtifactDownloadBegin, ArtifactDownloads, ArtifactIndex, ArtifactPreview,
    DownloadChunk, FileArtifactSource,
};
use crate::features::sessions::{SessionKind, SessionStore};

const MAX_CAPTURE_FILES: usize = 512;
const MAX_CAPTURE_FILE_BYTES: u64 = 32 * 1024 * 1024;
const MAX_CAPTURE_TOTAL_BYTES: u64 = 256 * 1024 * 1024;
const MAX_TASK_ARTIFACTS: usize = 32;
const MAX_TASK_ARTIFACT_BYTES: u64 = 64 * 1024 * 1024;
const MAX_PROJECT_CAPTURE_FILES: usize = 4_096;
const MAX_PROJECT_CAPTURE_BYTES: u64 = 512 * 1024 * 1024;
const MAX_CODE_CAPTURES: usize = 64;

/// Managed state for artifact access (DEV-20 registers it).
pub(crate) struct DesktopRemoteArtifactRuntime {
    downloads: ArtifactDownloads,
    tasks: Arc<RemoteTaskService>,
    captures: Mutex<HashMap<String, TaskArtifactCapture>>,
    code_captures: Mutex<HashMap<String, Arc<CodeArtifactCapture>>>,
    pending_code_artifacts: Mutex<HashMap<String, PendingCodeArtifacts>>,
    /// Serializes visible publication with connection teardown. A teardown
    /// that wins first removes the capture; one that runs second removes every
    /// entry published before it, so artifacts cannot resurrect afterwards.
    publication_lock: Mutex<()>,
    // Invalidates scans started before teardown, including begin-capture.
    publication_revision: AtomicU64,
    #[cfg(test)]
    before_code_publish: Mutex<Option<Box<dyn FnOnce() + Send>>>,
}

#[derive(Clone, Eq, PartialEq)]
struct FileStamp {
    bytes: u64,
    sha256: String,
}

struct TaskArtifactCapture {
    task_sequence: u64,
    session_id: String,
    lease: TaskLeaseBinding,
    before: HashMap<PathBuf, FileStamp>,
}

struct TaskCaptureAttempt<'a> {
    runtime: &'a DesktopRemoteArtifactRuntime,
    task_id: &'a str,
    sequence: u64,
}

impl Drop for TaskCaptureAttempt<'_> {
    fn drop(&mut self) {
        let mut captures = self.runtime.captures.lock();
        if captures
            .get(self.task_id)
            .is_some_and(|capture| capture.task_sequence == self.sequence)
        {
            captures.remove(self.task_id);
        }
    }
}

struct CodeArtifactCapture {
    token: RemoteCodeCaptureToken,
    session_id: String,
    lease: TaskLeaseBinding,
    workspace_root: PathBuf,
    before: HashMap<PathBuf, FileStamp>,
}

struct PendingCodeArtifacts {
    task_sequence: u64,
    session_id: String,
    lease: TaskLeaseBinding,
    copied: Vec<PathBuf>,
    events: Arc<dyn ControllerTaskEventSink>,
}

// An off-lock candidate owns only its own unique private copies. Never clear
// the task-wide index here: a concurrent winner may already own that index.
struct PendingArtifactStage {
    copied: Vec<PathBuf>,
    committed: bool,
}

impl PendingArtifactStage {
    fn new(capacity: usize) -> Self {
        Self {
            copied: Vec::with_capacity(capacity),
            committed: false,
        }
    }

    fn commit(mut self) -> Vec<PathBuf> {
        self.committed = true;
        std::mem::take(&mut self.copied)
    }
}

impl Drop for PendingArtifactStage {
    fn drop(&mut self) {
        if self.committed {
            return;
        }
        remove_private_copies(&self.copied);
    }
}

// Failure removes only this attempt's token, never a replacement capture.
struct CodeCaptureAttempt<'a> {
    runtime: &'a DesktopRemoteArtifactRuntime,
    task_id: &'a str,
    token: Arc<CodeArtifactCapture>,
}

impl Drop for CodeCaptureAttempt<'_> {
    fn drop(&mut self) {
        let mut captures = self.runtime.code_captures.lock();
        if captures
            .get(self.task_id)
            .is_some_and(|current| Arc::ptr_eq(current, &self.token))
        {
            captures.remove(self.task_id);
        }
    }
}

impl DesktopRemoteArtifactRuntime {
    #[cfg(test)]
    fn begin_test_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        workspace: &Path,
    ) -> Result<RemoteCodeCaptureToken, String> {
        let sequence = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?
            .sequence;
        self.begin_code_capture(lease, session_id, task_id, workspace, sequence)
    }

    #[cfg(test)]
    fn finish_test_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        events: Arc<dyn ControllerTaskEventSink>,
        publication_allowed: &(dyn Fn() -> Result<(), String> + Sync),
    ) -> Result<Vec<ArtifactDescriptor>, String> {
        let token = self
            .code_captures
            .lock()
            .get(task_id)
            .ok_or_else(|| "artifact_capture_missing".to_owned())?
            .token
            .clone();
        self.finish_code_capture(
            lease,
            session_id,
            task_id,
            events,
            &token,
            publication_allowed,
        )
    }

    #[cfg(test)]
    fn discard_test_code_capture(&self, lease: &LeaseContext, session_id: &str, task_id: &str) {
        let token = self
            .code_captures
            .lock()
            .get(task_id)
            .unwrap()
            .token
            .clone();
        self.discard_code_capture(lease, session_id, task_id, &token);
    }

    pub(crate) fn new(downloads: ArtifactDownloads, tasks: Arc<RemoteTaskService>) -> Arc<Self> {
        Arc::new(Self {
            downloads,
            tasks,
            captures: Mutex::new(HashMap::new()),
            code_captures: Mutex::new(HashMap::new()),
            pending_code_artifacts: Mutex::new(HashMap::new()),
            publication_lock: Mutex::new(()),
            publication_revision: AtomicU64::new(0),
            #[cfg(test)]
            before_code_publish: Mutex::new(None),
        })
    }

    pub(crate) fn with_registry(
        tasks: Arc<RemoteTaskService>,
        registry: Arc<TransferRegistry>,
    ) -> Arc<Self> {
        let runtime = Self::new(
            ArtifactDownloads::new(registry, Arc::new(ArtifactIndex::default())),
            tasks,
        );
        // DEV-18 审核修复：未完成下载同样周期性回收（RC-F-003 下载侧）。
        let weak = Arc::downgrade(&runtime);
        tauri::async_runtime::spawn(async move {
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(1));
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                interval.tick().await;
                let Some(owner) = weak.upgrade() else {
                    break;
                };
                let now = monotonic_millis();
                if tokio::task::spawn_blocking(move || owner.downloads.expire(now))
                    .await
                    .is_err()
                {
                    eprintln!("[remote-control] artifact_reaper_failed");
                }
            }
        });
        runtime
    }

    pub(crate) fn downloads(&self) -> &ArtifactDownloads {
        &self.downloads
    }

    /// Capture the B-authoritative session's private output inventory before
    /// executor dispatch. A never supplies paths or task attribution.
    pub(crate) fn begin_task_capture(
        &self,
        store: &SessionStore,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        task_sequence: u64,
        publication_allowed: &dyn Fn() -> Result<(), String>,
    ) -> Result<(), String> {
        publication_allowed()?;
        let revision = self.publication_revision.load(Ordering::Acquire);
        if self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?
            .sequence
            != task_sequence
        {
            return Err("artifact_capture_binding_mismatch".into());
        }
        self.require_task_record(lease, session_id, task_id, false)?;
        let before = private_artifact_files(store, session_id)?;
        let _publication = self.publication_lock.lock();
        if revision == u64::MAX || self.publication_revision.load(Ordering::Acquire) != revision {
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        self.require_task_record(lease, session_id, task_id, false)?;
        let mut captures = self.captures.lock();
        publication_allowed()?;
        if self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?
            .sequence
            != task_sequence
        {
            return Err("artifact_capture_binding_mismatch".into());
        }
        if let Some(existing) = captures.get(task_id) {
            if existing.session_id == session_id
                && existing.task_sequence == task_sequence
                && existing.lease == TaskLeaseBinding::from_context(lease)
            {
                return Ok(());
            }
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        captures.insert(
            task_id.to_owned(),
            TaskArtifactCapture {
                task_sequence,
                session_id: session_id.to_owned(),
                lease: TaskLeaseBinding::from_context(lease),
                before,
            },
        );
        Ok(())
    }

    /// Register only new/changed files after the same B task is terminal.
    /// The result contains opaque IDs and safe metadata; paths stay B-local.
    pub(crate) fn finish_task_capture(
        &self,
        store: &SessionStore,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        task_sequence: u64,
        publication_allowed: &dyn Fn() -> Result<(), String>,
    ) -> Result<Vec<ArtifactDescriptor>, String> {
        let _attempt = TaskCaptureAttempt {
            runtime: self,
            task_id,
            sequence: task_sequence,
        };
        publication_allowed()?;
        let after = private_artifact_files(store, session_id)?;
        self.tasks
            .ledger()
            .with_task_resources(task_id, task_sequence, || {
                let _publication = self.publication_lock.lock();
                publication_allowed()?;
                let record = self
                    .tasks
                    .status(task_id)
                    .map_err(|error| error.code.to_owned())?;
                if record.sequence != task_sequence {
                    return Err("artifact_capture_binding_mismatch".into());
                }
                if record.session_id != session_id
                    || record.lease != TaskLeaseBinding::from_context(lease)
                    || record.state != RemoteTaskState::Succeeded
                    || record.outcome_unknown
                {
                    self.captures.lock().remove(task_id);
                    return Err("artifact_capture_binding_mismatch".to_owned());
                }
                let capture = {
                    let mut captures = self.captures.lock();
                    let capture = captures
                        .get(task_id)
                        .ok_or_else(|| "artifact_capture_missing".to_owned())?;
                    if capture.session_id != session_id
                        || capture.task_sequence != task_sequence
                        || capture.lease != TaskLeaseBinding::from_context(lease)
                    {
                        return Err("artifact_capture_binding_mismatch".to_owned());
                    }
                    captures
                        .remove(task_id)
                        .ok_or_else(|| "artifact_capture_missing".to_owned())?
                };
                let mut changed: Vec<_> = after
                    .into_iter()
                    .filter(|(path, stamp)| capture.before.get(path) != Some(stamp))
                    .collect();
                changed.sort_by(|left, right| left.0.cmp(&right.0));
                if changed.len() > MAX_TASK_ARTIFACTS
                    || changed.iter().map(|(_, stamp)| stamp.bytes).sum::<u64>()
                        > MAX_TASK_ARTIFACT_BYTES
                {
                    return Err("artifact_capture_capacity".to_owned());
                }
                let mut registered = Vec::with_capacity(changed.len());
                for (path, stamp) in changed {
                    let result = (|| {
                        let name = path
                            .file_name()
                            .and_then(|name| name.to_str())
                            .map(str::to_owned)
                            .ok_or_else(|| "artifact_name_invalid".to_owned())?;
                        let id = format!("artifact-{}", uuid::Uuid::new_v4().simple());
                        index_artifact_owned(
                            self,
                            lease,
                            session_id,
                            task_id,
                            &id,
                            &name,
                            mime_for_artifact(&path),
                            &stamp.sha256,
                            path,
                            task_sequence,
                        )
                    })();
                    match result {
                        Ok(descriptor) => registered.push(descriptor),
                        Err(error) => {
                            self.downloads.index().clear_admission(task_sequence);
                            return Err(error);
                        }
                    }
                }
                Ok(registered)
            })
            .map_err(|error| error.code.to_owned())?
    }

    /// Return only artifacts whose owning task is terminal under the exact
    /// authenticated lease/session binding.
    pub(crate) fn artifact_index(
        &self,
        lease: &LeaseContext,
        session_id: &str,
    ) -> Result<Vec<ArtifactDescriptor>, String> {
        if !self
            .tasks
            .is_remote_session_for(session_id, &lease.remote_control_generation)
        {
            return Err("task_session_binding_mismatch".to_owned());
        }
        Ok(self
            .downloads
            .index()
            .list_for_binding(session_id, lease)
            .into_iter()
            .filter(|descriptor| {
                self.require_task_record(lease, session_id, &descriptor.task_id, true)
                    .is_ok()
                    && self
                        .require_artifact_admission(&descriptor.artifact_id, &descriptor.task_id)
                        .is_ok()
            })
            .collect())
    }

    pub(crate) fn artifact_preview(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        artifact_id: &str,
    ) -> Result<ArtifactPreview, String> {
        let descriptor =
            self.downloads
                .index()
                .descriptor_for_binding(session_id, artifact_id, lease)?;
        self.require_task_record(lease, session_id, &descriptor.task_id, true)?;
        self.require_artifact_admission(artifact_id, &descriptor.task_id)?;
        self.downloads.index().preview_for_task_binding(
            session_id,
            artifact_id,
            &descriptor.task_id,
            lease,
        )
    }

    pub(crate) fn artifact_download_begin(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        now_b: u64,
    ) -> Result<ArtifactDownloadBegin, String> {
        let sequence = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?
            .sequence;
        self.tasks
            .ledger()
            .with_task_resources(task_id, sequence, || {
                self.require_task_record(lease, session_id, task_id, true)?;
                self.require_artifact_admission(artifact_id, task_id)?;
                self.downloads
                    .begin(lease, session_id, task_id, artifact_id, now_b)
            })
            .map_err(|error| error.code.to_owned())?
    }

    #[allow(clippy::too_many_arguments)]
    pub(crate) fn artifact_download_chunk(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        handle_id: &str,
        chunk_index: u64,
        now_b: u64,
    ) -> Result<Option<DownloadChunk>, String> {
        let sequence = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?
            .sequence;
        self.tasks
            .ledger()
            .with_task_resources(task_id, sequence, || {
                self.require_task_record(lease, session_id, task_id, true)?;
                self.require_artifact_admission(artifact_id, task_id)?;
                self.downloads.next_chunk_bound(
                    lease,
                    session_id,
                    task_id,
                    artifact_id,
                    handle_id,
                    chunk_index,
                    now_b,
                )
            })
            .map_err(|error| error.code.to_owned())?
    }

    pub(crate) fn artifact_download_abort(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        artifact_id: &str,
        handle_id: &str,
    ) -> Result<bool, String> {
        let sequence = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?
            .sequence;
        self.tasks
            .ledger()
            .with_task_resources(task_id, sequence, || {
                self.require_task_record(lease, session_id, task_id, true)?;
                self.require_artifact_admission(artifact_id, task_id)?;
                self.downloads
                    .abort_bound(lease, session_id, task_id, artifact_id, handle_id)
            })
            .map_err(|error| error.code.to_owned())?
    }

    pub(crate) fn clear_connection(&self, connection_id: &str) {
        let _publication = self.publication_lock.lock();
        let mut revision = self.publication_revision.load(Ordering::Acquire);
        loop {
            match self.publication_revision.compare_exchange_weak(
                revision,
                revision.saturating_add(1),
                Ordering::AcqRel,
                Ordering::Acquire,
            ) {
                Ok(_) => break,
                Err(current) => revision = current,
            }
        }
        self.captures
            .lock()
            .retain(|_, capture| capture.lease.connection_id != connection_id);
        self.code_captures
            .lock()
            .retain(|_, capture| capture.lease.connection_id != connection_id);
        // Serialize pending removal with terminal publication. If a successful
        // terminal won first, the index cleanup below removes its publication;
        // if disconnect won first, commit can no longer find staged state.
        let mut pending = self.pending_code_artifacts.lock();
        let task_ids = pending
            .iter()
            .filter(|(_, artifacts)| artifacts.lease.connection_id == connection_id)
            .map(|(task_id, _)| task_id.clone())
            .collect::<Vec<_>>();
        for task_id in task_ids {
            if let Some(artifacts) = pending.remove(&task_id) {
                remove_private_copies(&artifacts.copied);
            }
        }
        self.downloads.cleanup_for_connection(connection_id);
    }

    fn require_artifact_admission(&self, artifact_id: &str, task_id: &str) -> Result<(), String> {
        let record = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?;
        let descriptor = self.downloads.index().descriptor_for_binding(
            &record.session_id,
            artifact_id,
            &lease_context_from_binding(&record.lease),
        )?;
        if descriptor.task_id != task_id {
            return Err("artifact_not_found".into());
        }
        self.downloads
            .index()
            .require_admission(artifact_id, record.sequence)
    }

    fn require_running_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        task_sequence: u64,
    ) -> Result<(), String> {
        let record = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?;
        if record.sequence != task_sequence
            || record.session_id != session_id
            || record.lease != TaskLeaseBinding::from_context(lease)
            || record.mode != TASK_MODE_CODE
            || record.state != RemoteTaskState::Running
            || record.cancel_requested
            || record.outcome_unknown
        {
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        Ok(())
    }

    fn require_task_record(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        terminal: bool,
    ) -> Result<(), String> {
        if !self
            .tasks
            .is_remote_session_for(session_id, &lease.remote_control_generation)
        {
            return Err("task_session_binding_mismatch".to_owned());
        }
        let record = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?;
        let state_matches = if terminal {
            record.state == RemoteTaskState::Succeeded && !record.outcome_unknown
        } else {
            !record.is_terminal()
        };
        if record.session_id != session_id
            || record.lease != TaskLeaseBinding::from_context(lease)
            || !state_matches
        {
            return Err("task_session_binding_mismatch".to_owned());
        }
        Ok(())
    }
}

impl RemoteCodeArtifactPort for DesktopRemoteArtifactRuntime {
    fn begin_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        workspace: &Path,
        task_sequence: u64,
    ) -> Result<RemoteCodeCaptureToken, String> {
        let revision = self.publication_revision.load(Ordering::Acquire);
        let record = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?;
        if record.sequence != task_sequence
            || record.session_id != session_id
            || record.lease != TaskLeaseBinding::from_context(lease)
            || record.mode != TASK_MODE_CODE
            || record.workspace_id.is_none()
            || record.state != RemoteTaskState::Running
            || record.cancel_requested
        {
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        {
            let captures = self.code_captures.lock();
            if !captures.contains_key(task_id) && captures.len() >= MAX_CODE_CAPTURES {
                return Err("artifact_capture_capacity".to_owned());
            }
        }
        if self.pending_code_artifacts.lock().contains_key(task_id) {
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        let workspace_root = std::fs::canonicalize(workspace)
            .map_err(|_| "artifact_workspace_unavailable".to_owned())?;
        if !workspace_root.is_dir()
            || std::fs::symlink_metadata(workspace)
                .map_err(|_| "artifact_workspace_unavailable".to_owned())?
                .file_type()
                .is_symlink()
        {
            return Err("artifact_workspace_unavailable".to_owned());
        }
        let before = project_artifact_files(&workspace_root)?;
        let _publication = self.publication_lock.lock();
        if revision == u64::MAX || self.publication_revision.load(Ordering::Acquire) != revision {
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        self.require_running_code_capture(lease, session_id, task_id, task_sequence)?;
        if self.pending_code_artifacts.lock().contains_key(task_id) {
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        let mut captures = self.code_captures.lock();
        if let Some(existing) = captures.get(task_id) {
            if existing.session_id == session_id
                && existing.token.task_sequence == task_sequence
                && existing.lease == TaskLeaseBinding::from_context(lease)
                && existing.workspace_root == workspace_root
            {
                return Ok(existing.token.clone());
            }
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        if captures.len() >= MAX_CODE_CAPTURES {
            return Err("artifact_capture_capacity".to_owned());
        }
        let token = RemoteCodeCaptureToken::new(task_sequence);
        captures.insert(
            task_id.to_owned(),
            Arc::new(CodeArtifactCapture {
                token: token.clone(),
                session_id: session_id.to_owned(),
                lease: TaskLeaseBinding::from_context(lease),
                workspace_root,
                before,
            }),
        );
        Ok(token)
    }

    fn finish_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        events: Arc<dyn ControllerTaskEventSink>,
        token: &RemoteCodeCaptureToken,
        publication_allowed: &(dyn Fn() -> Result<(), String> + Sync),
    ) -> Result<Vec<ArtifactDescriptor>, String> {
        publication_allowed()?;
        let capture = {
            let captures = self.code_captures.lock();
            let capture = captures
                .get(task_id)
                .cloned()
                .ok_or_else(|| "artifact_capture_missing".to_owned())?;
            if capture.token != *token
                || capture.session_id != session_id
                || capture.lease != TaskLeaseBinding::from_context(lease)
            {
                return Err("artifact_capture_binding_mismatch".to_owned());
            }
            capture
        };
        let _attempt = CodeCaptureAttempt {
            runtime: self,
            task_id,
            token: Arc::clone(&capture),
        };
        let record = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?;
        if record.sequence != token.task_sequence
            || record.session_id != session_id
            || record.lease != TaskLeaseBinding::from_context(lease)
            || record.mode != TASK_MODE_CODE
            || record.state != RemoteTaskState::Running
            || record.cancel_requested
            || record.outcome_unknown
        {
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        let current_root = std::fs::canonicalize(&capture.workspace_root)
            .map_err(|_| "artifact_workspace_unavailable".to_owned())?;
        if current_root != capture.workspace_root {
            return Err("artifact_workspace_changed".to_owned());
        }
        let after = project_artifact_files(&current_root)?;
        let after_scan = self
            .tasks
            .status(task_id)
            .map_err(|error| error.code.to_owned())?;
        if after_scan.sequence != token.task_sequence
            || after_scan.session_id != session_id
            || after_scan.lease != TaskLeaseBinding::from_context(lease)
            || after_scan.state != RemoteTaskState::Running
            || after_scan.cancel_requested
            || after_scan.outcome_unknown
        {
            return Err("artifact_capture_binding_mismatch".to_owned());
        }
        let mut changed = after
            .into_iter()
            .filter(|(relative, stamp)| capture.before.get(relative) != Some(stamp))
            .collect::<Vec<_>>();
        changed.sort_by(|left, right| left.0.cmp(&right.0));
        let total_bytes = changed
            .iter()
            .try_fold(0u64, |total, (_, stamp)| total.checked_add(stamp.bytes))
            .ok_or_else(|| "artifact_capture_capacity".to_owned())?;
        if changed.len() > MAX_TASK_ARTIFACTS || total_bytes > MAX_TASK_ARTIFACT_BYTES {
            return Err("artifact_capture_capacity".to_owned());
        }

        let artifact_root = private_artifact_root(session_id)?;
        let mut registered = Vec::with_capacity(changed.len());
        let mut stage = PendingArtifactStage::new(changed.len());
        let mut candidates = Vec::with_capacity(changed.len());
        for (relative, stamp) in changed {
            let result = (|| {
                let source = current_root.join(&relative);
                let display_name = relative
                    .file_name()
                    .and_then(|name| name.to_str())
                    .map(str::to_owned)
                    .ok_or_else(|| "artifact_name_invalid".to_owned())?;
                let mime_type = safe_project_artifact_mime(&relative)
                    .ok_or_else(|| "artifact_type_unsupported".to_owned())?;
                let artifact_id = format!("artifact-{}", uuid::Uuid::new_v4().simple());
                let private_copy = copy_project_artifact(
                    &current_root,
                    &source,
                    &artifact_root,
                    &artifact_id,
                    &stamp,
                )?;
                stage.copied.push(private_copy.clone());
                Ok::<_, String>((
                    artifact_id,
                    display_name,
                    mime_type,
                    stamp.sha256,
                    private_copy,
                ))
            })();
            match result {
                Ok(candidate) => candidates.push(candidate),
                Err(error) => return Err(error),
            }
        }
        #[cfg(test)]
        {
            let hook = self.before_code_publish.lock().take();
            if let Some(hook) = hook {
                hook();
            }
        }
        self.tasks
            .ledger()
            .with_task_resources(task_id, token.task_sequence, || {
                let _publication = self.publication_lock.lock();
                publication_allowed()?;
                self.require_running_code_capture(lease, session_id, task_id, token.task_sequence)?;
                let mut captures = self.code_captures.lock();
                if !captures
                    .get(task_id)
                    .is_some_and(|current| Arc::ptr_eq(current, &capture))
                    || self.pending_code_artifacts.lock().contains_key(task_id)
                {
                    return Err("artifact_capture_binding_mismatch".to_owned());
                }
                // Exact token validation, index registration and pending ownership
                // commit share teardown/terminal's publication lock.
                captures.remove(task_id);
                drop(captures);
                for (artifact_id, display_name, mime_type, sha256, private_copy) in candidates {
                    match index_pending_artifact(
                        self,
                        lease,
                        session_id,
                        task_id,
                        &artifact_id,
                        &display_name,
                        mime_type,
                        &sha256,
                        private_copy,
                        token.task_sequence,
                    ) {
                        Ok(descriptor) => registered.push(descriptor),
                        Err(error) => {
                            self.downloads.index().clear_admission(token.task_sequence);
                            return Err(error);
                        }
                    }
                }
                if !registered.is_empty() {
                    let copied = stage.commit();
                    self.pending_code_artifacts.lock().insert(
                        task_id.to_owned(),
                        PendingCodeArtifacts {
                            task_sequence: token.task_sequence,
                            session_id: session_id.to_owned(),
                            lease: TaskLeaseBinding::from_context(lease),
                            copied,
                            events,
                        },
                    );
                }
                Ok(registered)
            })
            .map_err(|error| error.code.to_owned())?
    }

    fn discard_code_capture(
        &self,
        lease: &LeaseContext,
        session_id: &str,
        task_id: &str,
        token: &RemoteCodeCaptureToken,
    ) {
        let _publication = self.publication_lock.lock();
        let binding = TaskLeaseBinding::from_context(lease);
        let mut captures = self.code_captures.lock();
        if captures.get(task_id).is_some_and(|capture| {
            capture.token == *token && capture.session_id == session_id && capture.lease == binding
        }) {
            captures.remove(task_id);
        }
    }
}

impl RemoteTaskArtifactScope for DesktopRemoteArtifactRuntime {
    fn commit_task_artifacts(&self, record: &RemoteTaskRecord) {
        let _publication = self.publication_lock.lock();
        let mut pending = self.pending_code_artifacts.lock();
        let Some(staged) = pending.get(&record.task_id) else {
            return;
        };
        if staged.task_sequence != record.sequence
            || staged.session_id != record.session_id
            || staged.lease != record.lease
        {
            return;
        }
        let Some(staged) = pending.remove(&record.task_id) else {
            return;
        };
        if staged
            .events
            .check_artifact_budget(record.sequence)
            .is_err()
        {
            self.downloads.index().clear_admission(record.sequence);
            remove_private_copies(&staged.copied);
            return;
        }
        let lease = lease_context_from_binding(&record.lease);
        let descriptors = self.downloads.index().publish_admission(
            &record.session_id,
            &record.task_id,
            &lease,
            record.sequence,
        );
        if descriptors.len() != staged.copied.len() {
            self.downloads.index().clear_admission(record.sequence);
            remove_private_copies(&staged.copied);
            return;
        }
        // The trusted event sink takes the per-admission resource gate.
        // Direct scope calls must not hold publication/pending locks while
        // entering it (service cleanup acquires these in the opposite order).
        drop(pending);
        drop(_publication);
        for descriptor in descriptors {
            staged.events.emit_committed_artifact(
                record.sequence,
                &record.session_id,
                &record.task_id,
                artifact_journal_payload(descriptor),
            );
        }
    }

    fn discard_task_artifacts(&self, record: &RemoteTaskRecord) {
        let _publication = self.publication_lock.lock();
        let mut captures = self.code_captures.lock();
        if captures.get(&record.task_id).is_some_and(|capture| {
            capture.token.task_sequence == record.sequence
                && capture.session_id == record.session_id
                && capture.lease == record.lease
        }) {
            captures.remove(&record.task_id);
        }
        drop(captures);
        let mut pending = self.pending_code_artifacts.lock();
        if !pending.get(&record.task_id).is_some_and(|staged| {
            staged.task_sequence == record.sequence
                && staged.session_id == record.session_id
                && staged.lease == record.lease
        }) {
            return;
        }
        if let Some(staged) = pending.remove(&record.task_id) {
            self.downloads.index().clear_admission(record.sequence);
            remove_private_copies(&staged.copied);
        }
    }
}

fn lease_context_from_binding(binding: &TaskLeaseBinding) -> LeaseContext {
    LeaseContext {
        connection_id: binding.connection_id.clone(),
        controller_lease_id: binding.controller_lease_id.clone(),
        lease_epoch: binding.lease_epoch.clone(),
        relay_epoch: binding.relay_epoch.clone(),
        remote_control_generation: binding.remote_control_generation.clone(),
    }
}

fn remove_private_copies(paths: &[PathBuf]) {
    for path in paths {
        let _ = std::fs::remove_file(path);
    }
}

fn artifact_journal_payload(descriptor: ArtifactDescriptor) -> serde_json::Value {
    serde_json::json!({
        "artifact_id": descriptor.artifact_id,
        "display_name": descriptor.display_name,
        "mime_type": descriptor.mime_type,
        "total_bytes": descriptor.total_bytes,
        "sha256": descriptor.sha256,
        "preview_kind": descriptor.preview_kind,
    })
}

fn monotonic_millis() -> u64 {
    use std::sync::OnceLock;
    use std::time::Instant;
    static ORIGIN: OnceLock<Instant> = OnceLock::new();
    let origin = ORIGIN.get_or_init(Instant::now);
    u64::try_from(origin.elapsed().as_millis()).unwrap_or(u64::MAX)
}

fn private_artifact_files(
    store: &SessionStore,
    session_id: &str,
) -> Result<HashMap<PathBuf, FileStamp>, String> {
    if store
        .session_kind(session_id)
        .map_err(|_| "task_session_not_found".to_owned())?
        != SessionKind::Chat
    {
        return Err("task_session_not_found".to_owned());
    }
    let roots = store
        .session_roots(session_id)
        .map_err(|_| "task_session_not_found".to_owned())?;
    // Never scan `roots.execution`: code sessions may bind it to an arbitrary
    // user project. Only B's private session roots are eligible for projection.
    let allowed: Vec<PathBuf> = [
        roots.ledger,
        crate::platform::paths::session_artifacts_dir(session_id),
    ]
    .into_iter()
    .filter_map(|root| std::fs::canonicalize(root).ok())
    .collect();
    let paths = super::sessions::list_workspace_files_for_session(session_id, store)
        .map_err(|_| "artifact_inventory_unavailable".to_owned())?;
    if paths.len() > MAX_CAPTURE_FILES {
        return Err("artifact_capture_capacity".to_owned());
    }
    let mut files = HashMap::new();
    let mut total_bytes = 0u64;
    for path in paths {
        let path = PathBuf::from(path);
        let original = std::fs::symlink_metadata(&path)
            .map_err(|_| "artifact_inventory_unavailable".to_owned())?;
        if original.file_type().is_symlink() || !original.is_file() {
            continue;
        }
        let canonical = std::fs::canonicalize(&path)
            .map_err(|_| "artifact_inventory_unavailable".to_owned())?;
        if !allowed
            .iter()
            .any(|root| canonical.parent() == Some(root.as_path()))
        {
            continue;
        }
        let metadata = std::fs::metadata(&canonical)
            .map_err(|_| "artifact_inventory_unavailable".to_owned())?;
        if metadata.len() == 0 || metadata.len() > MAX_CAPTURE_FILE_BYTES {
            continue;
        }
        total_bytes = total_bytes
            .checked_add(metadata.len())
            .ok_or_else(|| "artifact_capture_capacity".to_owned())?;
        if total_bytes > MAX_CAPTURE_TOTAL_BYTES {
            return Err("artifact_capture_capacity".to_owned());
        }
        files.insert(
            canonical.clone(),
            FileStamp {
                bytes: metadata.len(),
                sha256: sha256_file(&canonical, metadata.len())?,
            },
        );
    }
    Ok(files)
}

fn sha256_file(path: &Path, expected_bytes: u64) -> Result<String, String> {
    use std::io::Read as _;
    let mut file = std::fs::File::open(path).map_err(|_| "artifact_unavailable".to_owned())?;
    if file
        .metadata()
        .map_err(|_| "artifact_unavailable".to_owned())?
        .len()
        != expected_bytes
    {
        return Err("artifact_changed".to_owned());
    }
    let mut hasher = Sha256::new();
    let mut total = 0u64;
    let mut buffer = [0u8; 64 * 1024];
    loop {
        let size = file
            .read(&mut buffer)
            .map_err(|_| "artifact_unavailable".to_owned())?;
        if size == 0 {
            break;
        }
        total = total.saturating_add(size as u64);
        if total > expected_bytes {
            return Err("artifact_changed".to_owned());
        }
        hasher.update(&buffer[..size]);
    }
    if total != expected_bytes {
        return Err("artifact_changed".to_owned());
    }
    Ok(hasher
        .finalize()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect())
}

fn project_artifact_files(root: &Path) -> Result<HashMap<PathBuf, FileStamp>, String> {
    let canonical_root =
        std::fs::canonicalize(root).map_err(|_| "artifact_workspace_unavailable".to_owned())?;
    let mut files = HashMap::new();
    let mut total_bytes = 0u64;
    for entry in WalkDir::new(&canonical_root)
        .follow_links(false)
        .into_iter()
        .filter_entry(|entry| {
            entry.depth() == 0
                || !entry.file_type().is_dir()
                || !matches!(
                    entry.file_name().to_str(),
                    Some(".git" | ".hg" | ".svn" | "node_modules" | "target" | ".venv")
                )
        })
    {
        let entry = entry.map_err(|_| "artifact_inventory_unavailable".to_owned())?;
        if entry.depth() == 0 {
            continue;
        }
        let path = entry.path();
        let link_metadata = std::fs::symlink_metadata(path)
            .map_err(|_| "artifact_inventory_unavailable".to_owned())?;
        if link_metadata.file_type().is_symlink() || !link_metadata.is_file() {
            continue;
        }
        let relative = path
            .strip_prefix(&canonical_root)
            .map_err(|_| "artifact_workspace_changed".to_owned())?
            .to_path_buf();
        if safe_project_artifact_mime(&relative).is_none() {
            continue;
        }
        if files.len() >= MAX_PROJECT_CAPTURE_FILES {
            return Err("artifact_capture_capacity".to_owned());
        }
        let canonical =
            std::fs::canonicalize(path).map_err(|_| "artifact_inventory_unavailable".to_owned())?;
        if !canonical.starts_with(&canonical_root) {
            return Err("artifact_workspace_escape".to_owned());
        }
        let metadata = std::fs::metadata(&canonical)
            .map_err(|_| "artifact_inventory_unavailable".to_owned())?;
        if metadata.len() == 0 {
            continue;
        }
        if metadata.len() > MAX_CAPTURE_FILE_BYTES {
            return Err("artifact_capture_capacity".to_owned());
        }
        total_bytes = total_bytes
            .checked_add(metadata.len())
            .ok_or_else(|| "artifact_capture_capacity".to_owned())?;
        if total_bytes > MAX_PROJECT_CAPTURE_BYTES {
            return Err("artifact_capture_capacity".to_owned());
        }
        files.insert(
            relative,
            FileStamp {
                bytes: metadata.len(),
                sha256: sha256_file(&canonical, metadata.len())?,
            },
        );
    }
    Ok(files)
}

fn private_artifact_root(session_id: &str) -> Result<PathBuf, String> {
    let root = crate::platform::paths::session_artifacts_dir(session_id);
    std::fs::create_dir_all(&root).map_err(|_| "artifact_private_copy_failed".to_owned())?;
    let link_metadata =
        std::fs::symlink_metadata(&root).map_err(|_| "artifact_private_copy_failed".to_owned())?;
    if link_metadata.file_type().is_symlink() || !link_metadata.is_dir() {
        return Err("artifact_private_copy_failed".to_owned());
    }
    std::fs::canonicalize(root).map_err(|_| "artifact_private_copy_failed".to_owned())
}

fn copy_project_artifact(
    workspace_root: &Path,
    source: &Path,
    artifact_root: &Path,
    artifact_id: &str,
    expected: &FileStamp,
) -> Result<PathBuf, String> {
    use std::io::{Read as _, Write as _};

    let before_link =
        std::fs::symlink_metadata(source).map_err(|_| "artifact_changed".to_owned())?;
    if before_link.file_type().is_symlink() || !before_link.is_file() {
        return Err("artifact_workspace_escape".to_owned());
    }
    let canonical_before =
        std::fs::canonicalize(source).map_err(|_| "artifact_changed".to_owned())?;
    if !canonical_before.starts_with(workspace_root) {
        return Err("artifact_workspace_escape".to_owned());
    }
    let extension = source
        .extension()
        .and_then(|value| value.to_str())
        .map(|value| value.to_ascii_lowercase())
        .unwrap_or_else(|| "txt".to_owned());
    let destination = artifact_root.join(format!("{artifact_id}.{extension}"));
    let temporary = artifact_root.join(format!(".{artifact_id}.part"));
    if destination.parent() != Some(artifact_root) || temporary.parent() != Some(artifact_root) {
        return Err("artifact_private_copy_failed".to_owned());
    }

    let result = (|| {
        let mut input =
            std::fs::File::open(&canonical_before).map_err(|_| "artifact_changed".to_owned())?;
        if input
            .metadata()
            .map_err(|_| "artifact_changed".to_owned())?
            .len()
            != expected.bytes
        {
            return Err("artifact_changed".to_owned());
        }
        let mut output = std::fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)
            .map_err(|_| "artifact_private_copy_failed".to_owned())?;
        let mut hasher = Sha256::new();
        let mut total = 0u64;
        let mut buffer = [0u8; 64 * 1024];
        loop {
            let size = input
                .read(&mut buffer)
                .map_err(|_| "artifact_changed".to_owned())?;
            if size == 0 {
                break;
            }
            total = total
                .checked_add(size as u64)
                .ok_or_else(|| "artifact_changed".to_owned())?;
            if total > expected.bytes || total > MAX_CAPTURE_FILE_BYTES {
                return Err("artifact_changed".to_owned());
            }
            hasher.update(&buffer[..size]);
            output
                .write_all(&buffer[..size])
                .map_err(|_| "artifact_private_copy_failed".to_owned())?;
        }
        output
            .sync_all()
            .map_err(|_| "artifact_private_copy_failed".to_owned())?;
        let digest = hasher
            .finalize()
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let after_link =
            std::fs::symlink_metadata(source).map_err(|_| "artifact_changed".to_owned())?;
        let canonical_after =
            std::fs::canonicalize(source).map_err(|_| "artifact_changed".to_owned())?;
        if after_link.file_type().is_symlink()
            || canonical_after != canonical_before
            || total != expected.bytes
            || digest != expected.sha256
        {
            return Err("artifact_changed".to_owned());
        }
        std::fs::rename(&temporary, &destination)
            .map_err(|_| "artifact_private_copy_failed".to_owned())?;
        Ok(())
    })();
    if let Err(error) = result {
        let _ = std::fs::remove_file(&temporary);
        let _ = std::fs::remove_file(&destination);
        return Err(error);
    }
    Ok(destination)
}

fn safe_project_artifact_mime(path: &Path) -> Option<&'static str> {
    if matches!(
        path.file_name()?.to_str()?.to_ascii_lowercase().as_str(),
        "dockerfile" | "makefile" | "justfile"
    ) {
        return Some("text/plain");
    }
    match path.extension()?.to_str()?.to_ascii_lowercase().as_str() {
        "txt" | "md" | "markdown" | "csv" | "log" | "rs" | "js" | "mjs" | "cjs" | "jsx" | "ts"
        | "tsx" | "py" | "go" | "java" | "kt" | "kts" | "c" | "cc" | "cpp" | "h" | "hpp" | "cs"
        | "rb" | "php" | "swift" | "scala" | "lua" | "css" | "scss" | "less" | "sql" | "sh"
        | "bash" | "zsh" | "fish" | "ps1" | "bat" | "cmd" | "ini" | "cfg" | "conf"
        | "properties" | "gradle" => Some("text/plain"),
        "html" | "htm" => Some("text/html"),
        "json" => Some("application/json"),
        "xml" => Some("application/xml"),
        "yaml" | "yml" => Some("application/x-yaml"),
        "toml" => Some("application/toml"),
        "png" => Some("image/png"),
        "jpg" | "jpeg" => Some("image/jpeg"),
        "webp" => Some("image/webp"),
        "svg" => Some("image/svg+xml"),
        "pdf" => Some("application/pdf"),
        "docx" => Some("application/vnd.openxmlformats-officedocument.wordprocessingml.document"),
        "xlsx" => Some("application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"),
        "pptx" => Some("application/vnd.openxmlformats-officedocument.presentationml.presentation"),
        "zip" => Some("application/zip"),
        _ => None,
    }
}

fn mime_for_artifact(path: &Path) -> &'static str {
    match path
        .extension()
        .and_then(|value| value.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase()
        .as_str()
    {
        "txt" | "md" | "markdown" | "csv" | "log" => "text/plain",
        "html" | "htm" => "text/html",
        "json" => "application/json",
        "xml" => "application/xml",
        "yaml" | "yml" => "application/x-yaml",
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "pdf" => "application/pdf",
        _ => "application/octet-stream",
    }
}

/// Indexes one session artifact. The caller supplies the authoritative file
/// location; it stays in B's index and is never projected.
#[cfg(test)]
fn index_artifact(
    runtime: &DesktopRemoteArtifactRuntime,
    lease: &LeaseContext,
    session_id: &str,
    task_id: &str,
    artifact_id: &str,
    display_name: &str,
    mime_type: &str,
    sha256: &str,
    path: PathBuf,
) -> Result<ArtifactDescriptor, String> {
    let sequence = runtime
        .tasks
        .status(task_id)
        .map_err(|error| error.code.to_owned())?
        .sequence;
    index_artifact_owned(
        runtime,
        lease,
        session_id,
        task_id,
        artifact_id,
        display_name,
        mime_type,
        sha256,
        path,
        sequence,
    )
}

fn index_artifact_owned(
    runtime: &DesktopRemoteArtifactRuntime,
    lease: &LeaseContext,
    session_id: &str,
    task_id: &str,
    artifact_id: &str,
    display_name: &str,
    mime_type: &str,
    sha256: &str,
    path: std::path::PathBuf,
    sequence: u64,
) -> Result<ArtifactDescriptor, String> {
    runtime
        .tasks
        .ledger()
        .with_task_resources(task_id, sequence, || {
            runtime.downloads.index().register_owned(
                lease,
                session_id,
                task_id,
                artifact_id,
                display_name,
                mime_type,
                sha256,
                Arc::new(FileArtifactSource::new(path)),
                sequence,
                true,
            )
        })
        .map_err(|error| error.code.to_owned())?
}

fn index_pending_artifact(
    runtime: &DesktopRemoteArtifactRuntime,
    lease: &LeaseContext,
    session_id: &str,
    task_id: &str,
    artifact_id: &str,
    display_name: &str,
    mime_type: &str,
    sha256: &str,
    path: PathBuf,
    sequence: u64,
) -> Result<ArtifactDescriptor, String> {
    runtime.downloads.index().register_owned(
        lease,
        session_id,
        task_id,
        artifact_id,
        display_name,
        mime_type,
        sha256,
        Arc::new(FileArtifactSource::new(path)),
        sequence,
        false,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::task::{
        RemoteSessionProvisioner, RemoteTaskLedger, RemoteTaskRequest, RemoteTaskTerminal,
    };
    use serde_json::json;

    struct NullEvents;

    impl ControllerTaskEventSink for NullEvents {
        fn emit(&self, _event: serde_json::Value) {}
    }

    fn null_events() -> Arc<dyn ControllerTaskEventSink> {
        Arc::new(NullEvents)
    }

    #[derive(Default)]
    struct RecordingEvents(Mutex<Vec<JournalKind>>);

    impl ControllerTaskEventSink for RecordingEvents {
        fn emit(&self, _event: serde_json::Value) {}

        fn emit_journal(
            &self,
            kind: JournalKind,
            _session_id: &str,
            _task_id: Option<&str>,
            _payload: serde_json::Value,
        ) {
            self.0.lock().push(kind);
        }
    }

    fn test_lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn runtime_with_tasks(tasks: Arc<RemoteTaskService>) -> Arc<DesktopRemoteArtifactRuntime> {
        DesktopRemoteArtifactRuntime::new(
            ArtifactDownloads::new(
                Arc::new(TransferRegistry::new(Default::default()).unwrap()),
                Arc::new(ArtifactIndex::default()),
            ),
            tasks,
        )
    }

    fn runtime() -> Arc<DesktopRemoteArtifactRuntime> {
        runtime_with_tasks(Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        ))))
    }

    #[test]
    fn concurrent_connection_cleanup_does_not_lose_publication_revisions() {
        let runtime = runtime();
        std::thread::scope(|scope| {
            for _ in 0..64 {
                let runtime = Arc::clone(&runtime);
                scope.spawn(move || runtime.clear_connection("connection-0001"));
            }
        });
        assert_eq!(runtime.publication_revision.load(Ordering::Acquire), 64);
    }

    #[test]
    fn connection_cleanup_saturates_publication_revision_without_wrapping() {
        let runtime = runtime();
        runtime
            .publication_revision
            .store(u64::MAX - 1, Ordering::Release);
        std::thread::scope(|scope| {
            for _ in 0..64 {
                let runtime = Arc::clone(&runtime);
                scope.spawn(move || runtime.clear_connection("connection-0001"));
            }
        });
        assert_eq!(
            runtime.publication_revision.load(Ordering::Acquire),
            u64::MAX
        );
        runtime.clear_connection("connection-0001");
        assert_eq!(
            runtime.publication_revision.load(Ordering::Acquire),
            u64::MAX
        );
    }

    struct ExistingSession(String);

    impl RemoteSessionProvisioner for ExistingSession {
        fn create_authoritative_session(
            &self,
            _title: &str,
            _code_mode: bool,
        ) -> std::result::Result<String, String> {
            Ok(self.0.clone())
        }

        fn session_exists(&self, session_id: &str) -> std::result::Result<bool, String> {
            Ok(session_id == self.0)
        }
    }

    fn bound_runtime(session_id: &str) -> Arc<DesktopRemoteArtifactRuntime> {
        let tasks = Arc::new(RemoteTaskService::new(Arc::new(
            RemoteTaskLedger::in_memory(),
        )));
        tasks.set_session_provisioner(Arc::new(ExistingSession(session_id.to_owned())));
        tasks
            .switch_remote_session(
                "remote-session-0001",
                session_id,
                &test_lease().remote_control_generation,
            )
            .unwrap();
        let runtime = runtime_with_tasks(Arc::clone(&tasks));
        tasks.hooks().set_artifact_scope(runtime.clone());
        runtime
    }

    fn input_digest(input: &serde_json::Value) -> String {
        let text = input["text"].as_str().unwrap();
        let attachments = input["attachment_ids"]
            .as_array()
            .unwrap()
            .iter()
            .map(|value| value.as_str().unwrap())
            .collect::<Vec<_>>()
            .join("\0");
        let skills = input["skill_ids"]
            .as_array()
            .unwrap()
            .iter()
            .map(|value| value.as_str().unwrap())
            .collect::<Vec<_>>()
            .join("\0");
        let mut hasher = Sha256::new();
        hasher.update(b"pinvou-controller-task-input-v1");
        for field in [text.as_bytes(), attachments.as_bytes(), skills.as_bytes()] {
            hasher.update((field.len() as u64).to_be_bytes());
            hasher.update(field);
        }
        hasher
            .finalize()
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }

    fn admit_task(runtime: &DesktopRemoteArtifactRuntime, session_id: &str, task_id: &str) {
        let input = json!({"text":"make a report","attachment_ids":[],"skill_ids":[]});
        runtime
            .tasks
            .ledger()
            .admit(
                &test_lease(),
                &RemoteTaskRequest {
                    lease: TaskLeaseBinding::from_context(&test_lease()),
                    task_id: task_id.to_owned(),
                    client_request_id: format!("request-{task_id}"),
                    session_id: session_id.to_owned(),
                    capability_revision: "1".into(),
                    mode: "chat".into(),
                    workspace_id: None,
                    model_source: "controlled_device".into(),
                    input_digest: input_digest(&input),
                    input,
                },
                1_000,
            )
            .unwrap();
    }

    fn admit_running_code_task(
        runtime: &DesktopRemoteArtifactRuntime,
        session_id: &str,
        task_id: &str,
    ) {
        let _ticket = admit_code_ticket(runtime, session_id, task_id);
        runtime
            .tasks
            .ledger()
            .begin_dispatch(task_id, 1_001)
            .unwrap();
    }

    fn admit_code_ticket(
        runtime: &DesktopRemoteArtifactRuntime,
        session_id: &str,
        task_id: &str,
    ) -> crate::features::remote_control::controller::task::RemoteTaskTicket {
        let input = json!({
            "text": "create a report",
            "attachment_ids": [],
            "skill_ids": [],
            "workspace_id": "workspace-0001",
        });
        let outcome = runtime
            .tasks
            .ledger()
            .admit(
                &test_lease(),
                &RemoteTaskRequest {
                    lease: TaskLeaseBinding::from_context(&test_lease()),
                    task_id: task_id.to_owned(),
                    client_request_id: format!("request-{task_id}"),
                    session_id: session_id.to_owned(),
                    capability_revision: "1".into(),
                    mode: TASK_MODE_CODE.into(),
                    workspace_id: Some("workspace-0001".into()),
                    model_source: "controlled_device".into(),
                    input_digest: input_digest(&input),
                    input,
                },
                1_000,
            )
            .unwrap();
        match outcome {
            crate::features::remote_control::controller::task::AdmissionOutcome::Admitted(
                ticket,
            ) => ticket,
            _ => panic!("fresh code admission expected"),
        }
    }

    #[test]
    fn indexing_a_missing_file_fails_closed() {
        let runtime = bound_runtime("session-0001");
        admit_task(&runtime, "session-0001", "task-0001");
        let directory = tempfile::tempdir().unwrap();
        let missing = directory.path().join("missing.txt");
        assert_eq!(
            index_artifact(
                &runtime,
                &test_lease(),
                "session-0001",
                "task-0001",
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
    fn retired_capture_token_cannot_touch_same_task_id_after_real_ledger_pruning() {
        let runtime = bound_runtime("session-reused-capture");
        let workspace = tempfile::tempdir().unwrap();
        let task_id = "task-reused-capture";
        admit_running_code_task(&runtime, "session-reused-capture", task_id);
        let old = runtime
            .begin_test_code_capture(
                &test_lease(),
                "session-reused-capture",
                task_id,
                workspace.path(),
            )
            .unwrap();
        runtime
            .tasks
            .ledger()
            .complete(task_id, RemoteTaskTerminal::Failed, None, 1_010)
            .unwrap();
        runtime.discard_code_capture(&test_lease(), "session-reused-capture", task_id, &old);
        for index in 0..256 {
            let filler = format!("task-prune-{index}");
            admit_task(&runtime, "session-reused-capture", &filler);
            runtime
                .tasks
                .ledger()
                .complete(&filler, RemoteTaskTerminal::Succeeded, None, 1_020)
                .unwrap();
        }
        assert!(runtime.tasks.status(task_id).is_err());
        admit_running_code_task(&runtime, "session-reused-capture", task_id);
        let new = runtime
            .begin_test_code_capture(
                &test_lease(),
                "session-reused-capture",
                task_id,
                workspace.path(),
            )
            .unwrap();
        assert_ne!(old.task_sequence, new.task_sequence);
        runtime.discard_code_capture(&test_lease(), "session-reused-capture", task_id, &old);
        assert_eq!(runtime.code_captures.lock()[task_id].token, new);
        assert_eq!(
            runtime
                .finish_code_capture(
                    &test_lease(),
                    "session-reused-capture",
                    task_id,
                    null_events(),
                    &old,
                    &|| Ok(()),
                )
                .unwrap_err(),
            "artifact_capture_binding_mismatch"
        );
        assert_eq!(
            runtime
                .begin_code_capture(
                    &test_lease(),
                    "session-reused-capture",
                    task_id,
                    workspace.path(),
                    old.task_sequence,
                )
                .unwrap_err(),
            "artifact_capture_binding_mismatch"
        );
        assert_eq!(runtime.code_captures.lock()[task_id].token, new);
    }

    #[test]
    fn deadline_recheck_after_real_code_copy_prevents_late_publication() {
        crate::platform::test_support::with_temp_home("pinvou3-code-deadline", || {
            let project = tempfile::tempdir().unwrap();
            let store =
                SessionStore::boot_at_test_dir(&crate::platform::paths::pinvou3_home()).unwrap();
            let session_id = store
                .create_new("model".into(), None, project.path().to_path_buf())
                .unwrap()
                .metadata
                .id;
            let runtime = bound_runtime(&session_id);
            admit_running_code_task(&runtime, &session_id, "task-deadline");
            let token = runtime
                .begin_test_code_capture(
                    &test_lease(),
                    &session_id,
                    "task-deadline",
                    project.path(),
                )
                .unwrap();
            let source = project.path().join("output.md");
            std::fs::write(&source, "original output").unwrap();
            let expired = Arc::new(std::sync::atomic::AtomicBool::new(false));
            let at_publication = expired.clone();
            *runtime.before_code_publish.lock() = Some(Box::new(move || {
                at_publication.store(true, Ordering::SeqCst);
            }));
            let publication = || {
                if expired.load(Ordering::SeqCst) {
                    Err("task_turn_timeout".to_owned())
                } else {
                    Ok(())
                }
            };
            assert_eq!(
                runtime
                    .finish_code_capture(
                        &test_lease(),
                        &session_id,
                        "task-deadline",
                        null_events(),
                        &token,
                        &publication,
                    )
                    .unwrap_err(),
                "task_turn_timeout"
            );
            assert_eq!(runtime.downloads.index().len(), 0);
            assert!(runtime.pending_code_artifacts.lock().is_empty());
            assert!(runtime.code_captures.lock().is_empty());
            assert_eq!(
                std::fs::read_dir(private_artifact_root(&session_id).unwrap())
                    .unwrap()
                    .count(),
                0
            );
            assert_eq!(std::fs::read_to_string(source).unwrap(), "original output");
        });
    }

    #[test]
    fn real_dispatch_commits_one_code_artifact_and_fences_late_executor_events() {
        use crate::features::remote_control::controller::task::ControllerTaskExecutor;
        struct Output {
            artifacts: AtomicU64,
            errors: AtomicU64,
        }
        impl ControllerTaskEventSink for Output {
            fn emit(&self, _: serde_json::Value) {}
            fn emit_journal(
                &self,
                kind: JournalKind,
                _: &str,
                _: Option<&str>,
                _: serde_json::Value,
            ) {
                match kind {
                    JournalKind::Artifact => {
                        self.artifacts.fetch_add(1, Ordering::SeqCst);
                    }
                    JournalKind::Error => {
                        self.errors.fetch_add(1, Ordering::SeqCst);
                    }
                    _ => {}
                }
            }
        }
        struct Executor {
            artifacts: Arc<DesktopRemoteArtifactRuntime>,
            workspace: PathBuf,
            late: Mutex<Option<Arc<dyn ControllerTaskEventSink>>>,
        }
        #[async_trait::async_trait]
        impl ControllerTaskExecutor for Executor {
            async fn execute(
                &self,
                request: serde_json::Value,
                events: Arc<dyn ControllerTaskEventSink>,
                _: tokio_util::sync::CancellationToken,
            ) -> Result<(), String> {
                let task = request["task_id"].as_str().unwrap();
                let session = request["session_id"].as_str().unwrap();
                let sequence = self.artifacts.tasks.status(task).unwrap().sequence;
                let token = self.artifacts.begin_code_capture(
                    &test_lease(),
                    session,
                    task,
                    &self.workspace,
                    sequence,
                )?;
                std::fs::write(self.workspace.join("output.md"), "new code result").unwrap();
                self.artifacts.finish_code_capture(
                    &test_lease(),
                    session,
                    task,
                    events.clone(),
                    &token,
                    &|| Ok(()),
                )?;
                *self.late.lock() = Some(events);
                Ok(())
            }
        }
        crate::platform::test_support::with_temp_home("pinvou3-code-dispatch", || {
            let project = tempfile::tempdir().unwrap();
            let store =
                SessionStore::boot_at_test_dir(&crate::platform::paths::pinvou3_home()).unwrap();
            let session = store
                .create_new("model".into(), None, project.path().into())
                .unwrap()
                .metadata
                .id;
            let artifacts = bound_runtime(&session);
            let ticket = admit_code_ticket(&artifacts, &session, "task-code-dispatch");
            let executor = Arc::new(Executor {
                artifacts: artifacts.clone(),
                workspace: project.path().into(),
                late: Mutex::new(None),
            });
            artifacts
                .tasks
                .register_executor(TASK_MODE_CODE, executor.clone());
            let output = Arc::new(Output {
                artifacts: AtomicU64::new(0),
                errors: AtomicU64::new(0),
            });
            let record = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap()
                .block_on(artifacts.tasks.dispatch(
                    ticket,
                    output.clone(),
                    tokio_util::sync::CancellationToken::new(),
                    1_001,
                ))
                .unwrap();
            assert_eq!(record.state, RemoteTaskState::Succeeded);
            assert_eq!(output.artifacts.load(Ordering::SeqCst), 1);
            assert_eq!(
                artifacts
                    .artifact_index(&test_lease(), &session)
                    .unwrap()
                    .len(),
                1
            );
            artifacts.commit_task_artifacts(&record);
            assert_eq!(output.artifacts.load(Ordering::SeqCst), 1);
            executor.late.lock().as_ref().unwrap().emit_journal(
                JournalKind::Error,
                &session,
                Some("task-code-dispatch"),
                serde_json::json!({"code":"late_error"}),
            );
            assert_eq!(output.errors.load(Ordering::SeqCst), 0);
        });
    }

    #[test]
    fn old_terminal_hooks_cannot_touch_new_real_pending_copies_or_downloads_after_pruning() {
        crate::platform::test_support::with_temp_home("pinvou3-code-reused-pending", || {
            let project = tempfile::tempdir().unwrap();
            let store =
                SessionStore::boot_at_test_dir(&crate::platform::paths::pinvou3_home()).unwrap();
            let session = store
                .create_new("model".into(), None, project.path().into())
                .unwrap()
                .metadata
                .id;
            let runtime = bound_runtime(&session);
            let task = "task-pending-reused";
            admit_running_code_task(&runtime, &session, task);
            let old_sequence = runtime.tasks.status(task).unwrap().sequence;
            let old_file = project.path().join("old.md");
            std::fs::write(&old_file, "old artifact").unwrap();
            index_artifact_owned(
                &runtime,
                &test_lease(),
                &session,
                task,
                "artifact-old-instance",
                "old.md",
                "text/markdown",
                &sha256_file(&old_file, 12).unwrap(),
                old_file,
                old_sequence,
            )
            .unwrap();
            let old = runtime
                .tasks
                .ledger()
                .complete(task, RemoteTaskTerminal::Succeeded, None, 1_010)
                .unwrap();
            let old_download = runtime
                .artifact_download_begin(
                    &test_lease(),
                    &session,
                    task,
                    "artifact-old-instance",
                    1_011,
                )
                .unwrap();
            for index in 0..256 {
                let filler = format!("task-pending-prune-{index}");
                admit_task(&runtime, &session, &filler);
                runtime
                    .tasks
                    .ledger()
                    .complete(&filler, RemoteTaskTerminal::Succeeded, None, 1_020)
                    .unwrap();
            }
            assert!(runtime.tasks.status(task).is_err());
            admit_running_code_task(&runtime, &session, task);
            let token = runtime
                .begin_test_code_capture(&test_lease(), &session, task, project.path())
                .unwrap();
            std::fs::write(project.path().join("new.md"), "new artifact").unwrap();
            let descriptors = runtime
                .finish_code_capture(
                    &test_lease(),
                    &session,
                    task,
                    null_events(),
                    &token,
                    &|| Ok(()),
                )
                .unwrap();
            assert_eq!(descriptors.len(), 1);
            let copies = runtime.pending_code_artifacts.lock()[task].copied.clone();
            assert_eq!(copies.len(), 1);
            runtime.commit_task_artifacts(&old);
            runtime.discard_task_artifacts(&old);
            assert_eq!(
                runtime.pending_code_artifacts.lock()[task].task_sequence,
                token.task_sequence
            );
            assert!(copies.iter().all(|path| path.is_file()));
            let new = runtime
                .tasks
                .ledger()
                .complete(task, RemoteTaskTerminal::Succeeded, None, 2_000)
                .unwrap();
            runtime.commit_task_artifacts(&new);
            let visible = runtime.artifact_index(&test_lease(), &session).unwrap();
            assert_eq!(visible.len(), 1);
            assert_eq!(visible[0].artifact_id, descriptors[0].artifact_id);
            assert_eq!(
                runtime
                    .artifact_preview(&test_lease(), &session, "artifact-old-instance")
                    .unwrap_err(),
                "artifact_binding_stale"
            );
            assert_eq!(
                runtime
                    .artifact_download_begin(
                        &test_lease(),
                        &session,
                        task,
                        "artifact-old-instance",
                        2_001
                    )
                    .unwrap_err(),
                "artifact_binding_stale"
            );
            assert_eq!(
                runtime
                    .artifact_download_chunk(
                        &test_lease(),
                        &session,
                        task,
                        "artifact-old-instance",
                        &old_download.handle_id,
                        0,
                        2_001
                    )
                    .unwrap_err(),
                "artifact_binding_stale"
            );
            let download = runtime
                .artifact_download_begin(
                    &test_lease(),
                    &session,
                    task,
                    &descriptors[0].artifact_id,
                    2_002,
                )
                .unwrap();
            runtime.commit_task_artifacts(&old);
            runtime.discard_task_artifacts(&old);
            assert_eq!(runtime.downloads.cleanup_for_admission(old.sequence), 1);
            assert!(copies.iter().all(|path| path.is_file()));
            let chunk = runtime
                .artifact_download_chunk(
                    &test_lease(),
                    &session,
                    task,
                    &descriptors[0].artifact_id,
                    &download.handle_id,
                    0,
                    2_003,
                )
                .unwrap()
                .unwrap();
            assert_eq!(chunk.bytes, b"new artifact");
            assert!(chunk.completed.is_some());
            assert_eq!(
                runtime
                    .artifact_index(&test_lease(), &session)
                    .unwrap()
                    .len(),
                1
            );
        });
    }

    #[test]
    fn chat_capture_rechecks_frozen_deadline_after_real_scan_and_releases_only_its_attempt() {
        crate::platform::test_support::with_temp_home("pinvou3-chat-capture-budget", || {
            let project = tempfile::tempdir().unwrap();
            let store =
                SessionStore::boot_at_test_dir(&crate::platform::paths::pinvou3_home()).unwrap();
            let session = store
                .create_new("model".into(), None, project.path().into())
                .unwrap()
                .metadata
                .id;
            let runtime = bound_runtime(&session);
            let task = "task-chat-deadline";
            admit_task(&runtime, &session, task);
            let sequence = runtime.tasks.status(task).unwrap().sequence;
            runtime
                .begin_task_capture(&store, &test_lease(), &session, task, sequence, &|| Ok(()))
                .unwrap();
            let ledger = store.session_roots(&session).unwrap().ledger;
            std::fs::create_dir_all(&ledger).unwrap();
            let output = ledger.join("result.md");
            std::fs::write(&output, "completed chat output").unwrap();
            runtime
                .tasks
                .complete(task, RemoteTaskTerminal::Succeeded, None, 1_100)
                .unwrap();
            let clock = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .start_paused(true)
                .build()
                .unwrap();
            let _clock_context = clock.enter();
            let budget =
                crate::features::remote_control::controller::turn_budget::TurnBudget::for_test(
                    std::time::Duration::from_secs(1),
                );
            let checkpoints = AtomicU64::new(0);
            let publication = || {
                if checkpoints.fetch_add(1, Ordering::SeqCst) == 1 {
                    clock.block_on(tokio::time::advance(std::time::Duration::from_secs(2)));
                }
                budget.checkpoint(&tokio_util::sync::CancellationToken::new())
            };
            assert_eq!(
                runtime
                    .finish_task_capture(
                        &store,
                        &test_lease(),
                        &session,
                        task,
                        sequence,
                        &publication
                    )
                    .unwrap_err(),
                "task_turn_timeout"
            );
            assert_eq!(checkpoints.load(Ordering::SeqCst), 2);
            assert!(runtime.captures.lock().is_empty());
            assert_eq!(runtime.downloads.index().len(), 0);
            assert_eq!(
                std::fs::read_to_string(output).unwrap(),
                "completed chat output"
            );
        });
    }

    #[test]
    fn direct_artifact_commit_does_not_hold_publication_while_waiting_for_service_gate() {
        use std::sync::mpsc;
        use std::time::Duration;
        struct Output(AtomicU64);
        impl ControllerTaskEventSink for Output {
            fn emit(&self, _: serde_json::Value) {}
            fn emit_journal(
                &self,
                kind: JournalKind,
                _: &str,
                _: Option<&str>,
                _: serde_json::Value,
            ) {
                if kind == JournalKind::Artifact {
                    self.0.fetch_add(1, Ordering::SeqCst);
                }
            }
        }
        struct BeforeGate {
            entered: mpsc::Sender<()>,
            inner: Arc<dyn ControllerTaskEventSink>,
        }
        impl ControllerTaskEventSink for BeforeGate {
            fn emit(&self, _: serde_json::Value) {}
            fn emit_committed_artifact(
                &self,
                sequence: u64,
                session: &str,
                task: &str,
                payload: serde_json::Value,
            ) {
                self.entered.send(()).unwrap();
                self.inner
                    .emit_committed_artifact(sequence, session, task, payload);
            }
        }
        crate::platform::test_support::with_temp_home("pinvou3-direct-code-commit", || {
            let project = tempfile::tempdir().unwrap();
            let store =
                SessionStore::boot_at_test_dir(&crate::platform::paths::pinvou3_home()).unwrap();
            let session = store
                .create_new("model".into(), None, project.path().into())
                .unwrap()
                .metadata
                .id;
            let runtime = bound_runtime(&session);
            let task = "task-direct-code-commit";
            admit_running_code_task(&runtime, &session, task);
            let running = runtime.tasks.status(task).unwrap();
            let output = Arc::new(Output(AtomicU64::new(0)));
            let bounded = runtime
                .tasks
                .events_for_record(&running, output.clone(), None);
            let (entered_tx, entered_rx) = mpsc::channel();
            let events = Arc::new(BeforeGate {
                entered: entered_tx,
                inner: bounded,
            });
            let token = runtime
                .begin_test_code_capture(&test_lease(), &session, task, project.path())
                .unwrap();
            std::fs::write(project.path().join("output.md"), "bounded commit").unwrap();
            runtime
                .finish_code_capture(&test_lease(), &session, task, events, &token, &|| Ok(()))
                .unwrap();
            let terminal = runtime
                .tasks
                .ledger()
                .complete(task, RemoteTaskTerminal::Succeeded, None, 1_100)
                .unwrap();
            let (ready_tx, ready_rx) = mpsc::channel();
            let (done_tx, done_rx) = mpsc::channel();
            let service = runtime.tasks.clone();
            let sequence = terminal.sequence;
            let cleanup_done = done_tx.clone();
            let cleanup = std::thread::spawn(move || {
                service
                    .ledger()
                    .with_task_resources(task, sequence, || {
                        ready_tx.send(()).unwrap();
                        entered_rx.recv_timeout(Duration::from_secs(5)).unwrap();
                        service
                            .complete(task, RemoteTaskTerminal::Succeeded, None, 1_101)
                            .unwrap();
                    })
                    .unwrap();
                cleanup_done.send(()).unwrap();
            });
            ready_rx.recv_timeout(Duration::from_secs(5)).unwrap();
            let direct_runtime = runtime.clone();
            let direct = std::thread::spawn(move || {
                direct_runtime.commit_task_artifacts(&terminal);
                done_tx.send(()).unwrap();
            });
            done_rx.recv_timeout(Duration::from_secs(5)).unwrap();
            done_rx.recv_timeout(Duration::from_secs(5)).unwrap();
            cleanup.join().unwrap();
            direct.join().unwrap();
            assert_eq!(output.0.load(Ordering::SeqCst), 1);
            assert_eq!(
                runtime
                    .artifact_index(&test_lease(), &session)
                    .unwrap()
                    .len(),
                1
            );
        });
    }

    #[test]
    fn code_terminal_commit_after_frozen_deadline_revokes_staged_index_and_private_copies() {
        crate::platform::test_support::with_temp_home("pinvou3-code-terminal-budget", || {
            let project = tempfile::tempdir().unwrap();
            let store =
                SessionStore::boot_at_test_dir(&crate::platform::paths::pinvou3_home()).unwrap();
            let session = store
                .create_new("model".into(), None, project.path().into())
                .unwrap()
                .metadata
                .id;
            let runtime = bound_runtime(&session);
            let task = "task-code-terminal-deadline";
            admit_running_code_task(&runtime, &session, task);
            let record = runtime.tasks.status(task).unwrap();
            let clock = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .start_paused(true)
                .build()
                .unwrap();
            let _clock_context = clock.enter();
            let budget =
                crate::features::remote_control::controller::turn_budget::TurnBudget::for_test(
                    std::time::Duration::from_secs(1),
                );
            let events = runtime
                .tasks
                .events_for_record(&record, null_events(), Some(budget));
            let token = runtime
                .begin_test_code_capture(&test_lease(), &session, task, project.path())
                .unwrap();
            let source = project.path().join("output.md");
            std::fs::write(&source, "on-time staged output").unwrap();
            let descriptors = runtime
                .finish_code_capture(&test_lease(), &session, task, events, &token, &|| {
                    budget.checkpoint(&tokio_util::sync::CancellationToken::new())
                })
                .unwrap();
            assert_eq!(descriptors.len(), 1);
            let copies = runtime.pending_code_artifacts.lock()[task].copied.clone();
            assert!(copies.iter().all(|path| path.is_file()));
            clock.block_on(tokio::time::advance(std::time::Duration::from_secs(2)));
            runtime
                .tasks
                .complete(task, RemoteTaskTerminal::Succeeded, None, 1_100)
                .unwrap();
            assert_eq!(runtime.downloads.index().len(), 0);
            assert!(runtime.pending_code_artifacts.lock().is_empty());
            assert!(copies.iter().all(|path| !path.exists()));
            assert_eq!(
                std::fs::read_to_string(source).unwrap(),
                "on-time staged output"
            );
        });
    }

    #[test]
    fn indexing_an_existing_file_exposes_no_path() {
        let runtime = bound_runtime("session-0001");
        admit_task(&runtime, "session-0001", "task-0001");
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
            &test_lease(),
            "session-0001",
            "task-0001",
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

    #[test]
    fn artifact_access_requires_terminal_owner_and_rejects_cross_task() {
        let runtime = bound_runtime("session-0001");
        admit_task(&runtime, "session-0001", "task-0001");
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("report.md");
        std::fs::write(&path, b"# report").unwrap();
        index_artifact(
            &runtime,
            &test_lease(),
            "session-0001",
            "task-0001",
            "artifact-0001",
            "report.md",
            "text/markdown",
            &sha256_file(&path, 8).unwrap(),
            path,
        )
        .unwrap();

        assert!(
            runtime
                .artifact_index(&test_lease(), "session-0001")
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            runtime
                .artifact_preview(&test_lease(), "session-0001", "artifact-0001")
                .unwrap_err(),
            "task_session_binding_mismatch"
        );
        runtime
            .tasks
            .complete("task-0001", RemoteTaskTerminal::Succeeded, None, 1_100)
            .unwrap();
        assert_eq!(
            runtime
                .artifact_index(&test_lease(), "session-0001")
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            runtime
                .artifact_preview(&test_lease(), "session-0001", "artifact-0001")
                .unwrap()
                .text
                .as_deref(),
            Some("# report")
        );

        admit_task(&runtime, "session-0001", "task-0002");
        runtime
            .tasks
            .complete("task-0002", RemoteTaskTerminal::Succeeded, None, 1_200)
            .unwrap();
        assert_eq!(
            runtime
                .artifact_download_begin(
                    &test_lease(),
                    "session-0001",
                    "task-0002",
                    "artifact-0001",
                    1_300,
                )
                .unwrap_err(),
            "artifact_not_found"
        );
    }

    #[test]
    fn task_capture_indexes_only_start_to_terminal_private_file_changes() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let home = tempfile::tempdir().unwrap();
        let previous = std::env::var_os("PINVOU3_HOME");
        // SAFETY: platform::paths::tests::ENV_LOCK serializes process env writes.
        unsafe { std::env::set_var("PINVOU3_HOME", home.path()) };

        let store = SessionStore::boot_at_test_dir(home.path()).unwrap();
        let external = tempfile::tempdir().unwrap();
        let saved = store
            .create_new("model".into(), None, external.path().to_path_buf())
            .unwrap();
        let session_id = saved.metadata.id;
        let ledger = store.ledger_root(&session_id).unwrap();
        std::fs::create_dir_all(&ledger).unwrap();
        let existing = ledger.join("existing.txt");
        let unchanged = ledger.join("unchanged.txt");
        let outside = external.path().join("outside.txt");
        std::fs::write(&existing, b"hello").unwrap();
        std::fs::write(&unchanged, b"steady").unwrap();
        std::fs::write(&outside, b"outside").unwrap();
        let runtime = bound_runtime(&session_id);
        admit_task(&runtime, &session_id, "task-0001");
        runtime
            .begin_task_capture(
                &store,
                &test_lease(),
                &session_id,
                "task-0001",
                runtime.tasks.status("task-0001").unwrap().sequence,
                &|| Ok(()),
            )
            .unwrap();

        std::fs::write(&existing, b"world").unwrap();
        std::fs::write(ledger.join("created.md"), b"new").unwrap();
        std::fs::write(&outside, b"changed").unwrap();
        runtime
            .tasks
            .complete("task-0001", RemoteTaskTerminal::Succeeded, None, 1_100)
            .unwrap();
        let descriptors = runtime
            .finish_task_capture(
                &store,
                &test_lease(),
                &session_id,
                "task-0001",
                runtime.tasks.status("task-0001").unwrap().sequence,
                &|| Ok(()),
            )
            .unwrap();
        let mut names = descriptors
            .iter()
            .map(|descriptor| descriptor.display_name.as_str())
            .collect::<Vec<_>>();
        names.sort();
        assert_eq!(names, vec!["created.md", "existing.txt"]);
        let encoded = serde_json::to_string(&descriptors).unwrap();
        assert!(!encoded.contains(home.path().to_string_lossy().as_ref()));
        assert!(!encoded.contains(external.path().to_string_lossy().as_ref()));
        assert!(!encoded.contains("outside.txt"));

        match previous {
            Some(value) => {
                // SAFETY: platform::paths::tests::ENV_LOCK is still held.
                unsafe { std::env::set_var("PINVOU3_HOME", value) };
            }
            None => {
                // SAFETY: platform::paths::tests::ENV_LOCK is still held.
                unsafe { std::env::remove_var("PINVOU3_HOME") };
            }
        }
    }

    #[test]
    fn code_capture_copies_only_new_and_modified_project_files_into_private_artifacts() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let home = tempfile::tempdir().unwrap();
        let previous = std::env::var_os("PINVOU3_HOME");
        // SAFETY: platform::paths::tests::ENV_LOCK serializes process env writes.
        unsafe { std::env::set_var("PINVOU3_HOME", home.path()) };

        let store = SessionStore::boot_at_test_dir(home.path()).unwrap();
        let project = tempfile::tempdir().unwrap();
        let saved = store
            .create_new("model".into(), None, project.path().to_path_buf())
            .unwrap();
        let session_id = saved.metadata.id;
        let unchanged = project.path().join("unchanged.md");
        let modified = project.path().join("modified.md");
        let deleted = project.path().join("deleted.md");
        std::fs::write(&unchanged, b"old unchanged").unwrap();
        std::fs::write(&modified, b"old modified").unwrap();
        std::fs::write(&deleted, b"removed during turn").unwrap();
        let runtime = bound_runtime(&session_id);
        admit_running_code_task(&runtime, &session_id, "task-code-0001");
        runtime
            .begin_test_code_capture(&test_lease(), &session_id, "task-code-0001", project.path())
            .unwrap();

        std::fs::write(&modified, b"new modified").unwrap();
        std::fs::remove_file(&deleted).unwrap();
        std::fs::write(project.path().join("created.json"), br#"{"ok":true}"#).unwrap();
        std::fs::write(project.path().join("ignored.exe"), b"not an artifact").unwrap();
        let events = Arc::new(RecordingEvents::default());
        let event_port: Arc<dyn ControllerTaskEventSink> = events.clone();
        let descriptors = runtime
            .finish_test_code_capture(
                &test_lease(),
                &session_id,
                "task-code-0001",
                event_port,
                &|| Ok(()),
            )
            .unwrap();
        let mut names = descriptors
            .iter()
            .map(|descriptor| descriptor.display_name.as_str())
            .collect::<Vec<_>>();
        names.sort();
        assert_eq!(names, vec!["created.json", "modified.md"]);
        let encoded = serde_json::to_string(&descriptors).unwrap();
        assert!(!encoded.contains(project.path().to_string_lossy().as_ref()));
        assert!(!encoded.contains(home.path().to_string_lossy().as_ref()));
        assert!(!encoded.contains("unchanged.md"));
        assert!(!encoded.contains("deleted.md"));
        assert!(!encoded.contains("ignored.exe"));

        // The index is unavailable before task success, and afterwards reads
        // the immutable private copies rather than the mutable project files.
        assert!(
            runtime
                .artifact_index(&test_lease(), &session_id)
                .unwrap()
                .is_empty()
        );
        std::fs::write(&modified, b"changed again after capture").unwrap();
        std::fs::remove_file(project.path().join("created.json")).unwrap();
        runtime
            .tasks
            .complete("task-code-0001", RemoteTaskTerminal::Succeeded, None, 1_100)
            .unwrap();
        assert_eq!(
            events
                .0
                .lock()
                .iter()
                .filter(|kind| **kind == JournalKind::Artifact)
                .count(),
            2
        );
        let indexed = runtime.artifact_index(&test_lease(), &session_id).unwrap();
        assert_eq!(indexed.len(), 2);
        let modified_descriptor = indexed
            .iter()
            .find(|descriptor| descriptor.display_name == "modified.md")
            .unwrap();
        assert_eq!(
            runtime
                .artifact_preview(&test_lease(), &session_id, &modified_descriptor.artifact_id,)
                .unwrap()
                .text
                .as_deref(),
            Some("new modified")
        );

        match previous {
            Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
            None => unsafe { std::env::remove_var("PINVOU3_HOME") },
        }
    }

    #[test]
    fn code_capture_rejects_over_limit_output_and_never_indexes_cancelled_output() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let home = tempfile::tempdir().unwrap();
        let previous = std::env::var_os("PINVOU3_HOME");
        // SAFETY: platform::paths::tests::ENV_LOCK serializes process env writes.
        unsafe { std::env::set_var("PINVOU3_HOME", home.path()) };

        let store = SessionStore::boot_at_test_dir(home.path()).unwrap();
        let project = tempfile::tempdir().unwrap();
        let saved = store
            .create_new("model".into(), None, project.path().to_path_buf())
            .unwrap();
        let session_id = saved.metadata.id;
        let runtime = bound_runtime(&session_id);
        admit_running_code_task(&runtime, &session_id, "task-code-limit");
        runtime
            .begin_test_code_capture(
                &test_lease(),
                &session_id,
                "task-code-limit",
                project.path(),
            )
            .unwrap();
        let oversized = std::fs::File::create(project.path().join("oversized.pdf")).unwrap();
        oversized.set_len(MAX_CAPTURE_FILE_BYTES + 1).unwrap();
        assert_eq!(
            runtime
                .finish_test_code_capture(
                    &test_lease(),
                    &session_id,
                    "task-code-limit",
                    null_events(),
                    &|| Ok(())
                )
                .unwrap_err(),
            "artifact_capture_capacity"
        );
        assert_eq!(runtime.downloads.index().len(), 0);

        // A separate cancelled task may change the project, but cancellation
        // invalidates the capture before any private copy or index entry.
        runtime
            .tasks
            .complete("task-code-limit", RemoteTaskTerminal::Failed, None, 1_100)
            .unwrap();
        std::fs::remove_file(project.path().join("oversized.pdf")).unwrap();

        admit_running_code_task(&runtime, &session_id, "task-code-fail");
        runtime
            .begin_test_code_capture(&test_lease(), &session_id, "task-code-fail", project.path())
            .unwrap();
        let failed_output = project.path().join("failed.md");
        std::fs::write(&failed_output, b"must not be indexed").unwrap();
        runtime.discard_test_code_capture(&test_lease(), &session_id, "task-code-fail");
        runtime
            .tasks
            .complete("task-code-fail", RemoteTaskTerminal::Failed, None, 1_150)
            .unwrap();
        assert_eq!(runtime.downloads.index().len(), 0);
        std::fs::remove_file(failed_output).unwrap();

        admit_running_code_task(&runtime, &session_id, "task-code-cancel");
        runtime
            .begin_test_code_capture(
                &test_lease(),
                &session_id,
                "task-code-cancel",
                project.path(),
            )
            .unwrap();
        std::fs::write(project.path().join("cancelled.md"), b"must stay private").unwrap();
        runtime
            .tasks
            .ledger()
            .request_cancel("task-code-cancel", 1_200)
            .unwrap();
        assert_eq!(
            runtime
                .finish_test_code_capture(
                    &test_lease(),
                    &session_id,
                    "task-code-cancel",
                    null_events(),
                    &|| Ok(()),
                )
                .unwrap_err(),
            "artifact_capture_binding_mismatch"
        );
        assert_eq!(runtime.downloads.index().len(), 0);
        assert!(runtime.code_captures.lock().is_empty());

        match previous {
            Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
            None => unsafe { std::env::remove_var("PINVOU3_HOME") },
        }
    }

    #[test]
    fn staged_code_artifacts_are_removed_when_cancel_or_disconnect_wins_terminal_race() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let home = tempfile::tempdir().unwrap();
        let previous = std::env::var_os("PINVOU3_HOME");
        unsafe { std::env::set_var("PINVOU3_HOME", home.path()) };

        let store = SessionStore::boot_at_test_dir(home.path()).unwrap();
        let project = tempfile::tempdir().unwrap();
        let saved = store
            .create_new("model".into(), None, project.path().to_path_buf())
            .unwrap();
        let session_id = saved.metadata.id;
        let runtime = bound_runtime(&session_id);

        for (task_id, disconnect) in [
            ("task-code-cancel-race", false),
            ("task-code-disconnect-race", true),
        ] {
            admit_running_code_task(&runtime, &session_id, task_id);
            runtime
                .begin_test_code_capture(&test_lease(), &session_id, task_id, project.path())
                .unwrap();
            std::fs::write(
                project.path().join(format!("{task_id}.md")),
                b"private staged output",
            )
            .unwrap();
            let events = Arc::new(RecordingEvents::default());
            let event_port: Arc<dyn ControllerTaskEventSink> = events.clone();
            let staged = runtime
                .finish_test_code_capture(&test_lease(), &session_id, task_id, event_port, &|| {
                    Ok(())
                })
                .unwrap();
            assert_eq!(staged.len(), 1);
            assert!(
                runtime
                    .artifact_index(&test_lease(), &session_id)
                    .unwrap()
                    .is_empty()
            );

            if disconnect {
                runtime.clear_connection(&test_lease().connection_id);
                runtime.tasks.interrupt_generation(
                    &test_lease().remote_control_generation,
                    "connection_closed",
                    1_200,
                );
            } else {
                runtime.tasks.cancel(task_id, 1_200).unwrap();
                runtime
                    .tasks
                    .complete(task_id, RemoteTaskTerminal::Cancelled, None, 1_201)
                    .unwrap();
            }

            assert!(
                runtime
                    .artifact_index(&test_lease(), &session_id)
                    .unwrap()
                    .is_empty()
            );
            assert_eq!(runtime.downloads.index().len(), 0);
            assert!(!events.0.lock().contains(&JournalKind::Artifact));
            let private_root = private_artifact_root(&session_id).unwrap();
            assert_eq!(std::fs::read_dir(private_root).unwrap().count(), 0);
        }

        match previous {
            Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
            None => unsafe { std::env::remove_var("PINVOU3_HOME") },
        }
    }

    #[test]
    fn code_capture_registry_has_a_hard_capacity_limit() {
        let runtime = bound_runtime("session-capacity");
        let binding = TaskLeaseBinding::from_context(&test_lease());
        let workspace = tempfile::tempdir().unwrap();
        let root = std::fs::canonicalize(workspace.path()).unwrap();
        {
            let mut captures = runtime.code_captures.lock();
            for index in 0..MAX_CODE_CAPTURES {
                captures.insert(
                    format!("occupied-{index}"),
                    Arc::new(CodeArtifactCapture {
                        token: RemoteCodeCaptureToken::new(0),
                        session_id: "session-capacity".to_owned(),
                        lease: binding.clone(),
                        workspace_root: root.clone(),
                        before: HashMap::new(),
                    }),
                );
            }
        }
        admit_running_code_task(&runtime, "session-capacity", "task-code-over-capacity");
        assert_eq!(
            runtime
                .begin_test_code_capture(
                    &test_lease(),
                    "session-capacity",
                    "task-code-over-capacity",
                    workspace.path(),
                )
                .unwrap_err(),
            "artifact_capture_capacity"
        );
        assert_eq!(runtime.code_captures.lock().len(), MAX_CODE_CAPTURES);
    }

    #[test]
    fn off_lock_code_copy_cannot_resurrect_after_clear_or_erase_a_concurrent_winner() {
        let _guard = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let home = tempfile::tempdir().unwrap();
        let previous = std::env::var_os("PINVOU3_HOME");
        // SAFETY: the shared paths test lock serializes process env writes.
        unsafe { std::env::set_var("PINVOU3_HOME", home.path()) };
        let store = SessionStore::boot_at_test_dir(home.path()).unwrap();
        for disconnect in [true, false] {
            let project = tempfile::tempdir().unwrap();
            let session_id = store
                .create_new("model".into(), None, project.path().to_path_buf())
                .unwrap()
                .metadata
                .id;
            let runtime = bound_runtime(&session_id);
            let task_id = if disconnect {
                "task-copy-clear"
            } else {
                "task-copy-winner"
            };
            admit_running_code_task(&runtime, &session_id, task_id);
            runtime
                .begin_test_code_capture(&test_lease(), &session_id, task_id, project.path())
                .unwrap();
            let source = project.path().join("output.md");
            std::fs::write(&source, b"real private code output").unwrap();
            let copied = Arc::new(std::sync::Barrier::new(2));
            let resume = Arc::new(std::sync::Barrier::new(2));
            let copied_hook = Arc::clone(&copied);
            let resume_hook = Arc::clone(&resume);
            *runtime.before_code_publish.lock() = Some(Box::new(move || {
                copied_hook.wait();
                resume_hook.wait();
            }));
            let worker_runtime = Arc::clone(&runtime);
            let worker_session = session_id.clone();
            let worker = std::thread::spawn(move || {
                worker_runtime.finish_test_code_capture(
                    &test_lease(),
                    &worker_session,
                    task_id,
                    null_events(),
                    &|| Ok(()),
                )
            });
            copied.wait(); // actual copies exist, but no index ownership yet
            assert_eq!(runtime.downloads.index().len(), 0);
            if disconnect {
                runtime.clear_connection(&test_lease().connection_id);
            } else {
                // A second attempt wins the SAME capture token. The losing
                // candidate must clean only its own unique files, not this index.
                assert_eq!(
                    runtime
                        .finish_test_code_capture(
                            &test_lease(),
                            &session_id,
                            task_id,
                            null_events(),
                            &|| Ok(())
                        )
                        .unwrap()
                        .len(),
                    1
                );
            }
            resume.wait();
            assert_eq!(
                worker.join().unwrap().unwrap_err(),
                "artifact_capture_binding_mismatch"
            );
            assert!(runtime.code_captures.lock().is_empty());
            let count = if disconnect { 0 } else { 1 };
            assert_eq!(runtime.downloads.index().len(), count);
            assert_eq!(runtime.pending_code_artifacts.lock().len(), count);
            assert_eq!(
                std::fs::read_dir(private_artifact_root(&session_id).unwrap())
                    .unwrap()
                    .count(),
                count
            );
            assert_eq!(std::fs::read(&source).unwrap(), b"real private code output");
            runtime.clear_connection(&test_lease().connection_id);
        }
        match previous {
            Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
            None => unsafe { std::env::remove_var("PINVOU3_HOME") },
        }
    }

    #[test]
    fn project_inventory_never_follows_file_symlinks_outside_the_workspace() {
        let project = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        let secret = outside.path().join("secret.md");
        std::fs::write(&secret, b"secret").unwrap();
        let link = project.path().join("linked.md");
        #[cfg(windows)]
        let linked = std::os::windows::fs::symlink_file(&secret, &link).is_ok();
        #[cfg(unix)]
        let linked = std::os::unix::fs::symlink(&secret, &link).is_ok();
        #[cfg(not(any(windows, unix)))]
        let linked = false;
        if !linked {
            return;
        }
        let inventory = project_artifact_files(project.path()).unwrap();
        assert!(inventory.is_empty());
        assert!(
            !serde_json::to_string(&inventory.keys().collect::<Vec<_>>())
                .unwrap()
                .contains("secret.md")
        );
    }
}
