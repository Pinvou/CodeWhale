//! A→B attachment upload extension slot (implemented by DEV-17).
//!
//! The shared core owns ordering, budgets and digests. DEV-17 only implements
//! this port, which stages the already-accounted bytes into the B session
//! isolation directory. It receives no host path and no file name from A beyond
//! the bounded, slash-free metadata name.

use std::collections::{HashMap, HashSet};
use std::path::PathBuf;
use std::sync::Arc;

use parking_lot::Mutex;

use super::core::{
    CompletedTransfer, TransferBinding, TransferContext, TransferHandle, TransferMetadata,
};
pub(crate) use crate::features::files::file_ingest::PublishedAttachment;
use crate::features::remote_control::controller::protocol::LeaseContext;

struct CompletedReceipt {
    binding: TransferBinding,
    descriptor: PublishedAttachment,
    completed_at_ms: u64,
    consumed: bool,
}

#[derive(Default)]
struct UploadState {
    active: HashMap<String, TransferContext>,
    completed: HashMap<String, CompletedReceipt>,
    pending_cleanup: HashSet<String>,
}

const MAX_COMPLETED_RECEIPTS: usize = 64;

pub(crate) trait UploadStaging: Send + Sync {
    /// Allocates the staging target for one transfer.
    fn begin(
        &self,
        handle_id: &str,
        context: &TransferContext,
        metadata: &TransferMetadata,
    ) -> std::result::Result<(), String>;

    /// Appends one accepted chunk in order.
    fn accept_chunk(
        &self,
        handle_id: &str,
        chunk_index: u64,
        bytes: &[u8],
    ) -> std::result::Result<(), String>;

    /// Publishes the staged file once the core verified the digest.
    /// Return the absolute B-local published path; only this host port knows it.
    fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<PathBuf, String>;

    fn published_read_owner(
        &self,
        handle_id: &str,
    ) -> Option<Arc<dyn crate::features::files::file_ingest::AttachmentReadOwner>>;

    /// Drops the staging target. Must be idempotent.
    fn abort(&self, handle_id: &str) -> std::result::Result<(), String>;
}

/// Upload owner: binds the shared transfer core to one staging port. The
/// successful handle is consumed exactly once, and every failure path aborts
/// the staging target so no orphan cache survives.
pub(crate) struct AttachmentUploads {
    registry: Arc<super::core::TransferRegistry>,
    staging: Arc<dyn UploadStaging>,
    state: Mutex<UploadState>,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct UploadBegin {
    pub(crate) handle_id: String,
    pub(crate) chunk_bytes: usize,
    pub(crate) total_bytes: u64,
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum UploadProgress {
    Accepted {
        received_bytes: u64,
        next_chunk: u64,
    },
    Duplicate {
        received_bytes: u64,
    },
    Completed {
        sha256: String,
        bytes: u64,
    },
}

impl AttachmentUploads {
    pub(crate) fn new(
        registry: Arc<super::core::TransferRegistry>,
        staging: Arc<dyn UploadStaging>,
    ) -> Self {
        Self {
            registry,
            staging,
            state: Mutex::new(UploadState::default()),
        }
    }

    pub(crate) fn begin(
        &self,
        context: TransferContext,
        metadata: TransferMetadata,
        now_b: u64,
    ) -> std::result::Result<UploadBegin, String> {
        let mut state = self.state.lock();
        if context.direction != super::core::TransferDirection::Upload {
            return Err("transfer_direction_mismatch".to_owned());
        }
        let limits = self.registry.limits();
        let completed_bytes: u64 = state
            .completed
            .values()
            .filter(|receipt| receipt.binding.connection_id == context.binding.connection_id)
            .map(|receipt| receipt.descriptor.bytes)
            .sum();
        if state.completed.len() >= MAX_COMPLETED_RECEIPTS
            || completed_bytes
                .saturating_add(
                    self.registry
                        .connection_bytes(&context.binding.connection_id),
                )
                .saturating_add(metadata.total_bytes)
                > limits.max_connection_bytes
        {
            return Err("transfer_capacity".to_owned());
        }
        let task_count = state
            .active
            .values()
            .filter(|active| active.binding.task_id == context.binding.task_id)
            .count()
            + state
                .completed
                .values()
                .filter(|receipt| receipt.binding.task_id == context.binding.task_id)
                .count();
        if task_count >= limits.max_transfers_per_task {
            return Err("transfer_task_capacity".to_owned());
        }
        let (handle, progress) = self
            .registry
            .begin(context.clone(), metadata.clone(), now_b)
            .map_err(|error| error.code.to_owned())?;
        state.active.insert(handle.id().to_owned(), context.clone());
        if let Err(error) = self.staging.begin(handle.id(), &context, &metadata) {
            self.registry.abort(&handle);
            self.release_staging(&mut state, handle.id());
            return Err(error);
        }
        Ok(UploadBegin {
            handle_id: handle.id().to_owned(),
            chunk_bytes: self.registry.limits().chunk_bytes,
            total_bytes: progress.total_bytes,
        })
    }

    pub(crate) fn accept_chunk(
        &self,
        handle_id: &str,
        chunk_index: u64,
        bytes: &[u8],
        now_b: u64,
    ) -> std::result::Result<UploadProgress, String> {
        let handle = Self::handle(handle_id)?;
        let mut state = self.state.lock();
        if state.completed.contains_key(handle_id) {
            return Err("transfer_unknown".to_owned());
        }
        let outcome = match self.registry.accept_chunk(
            &handle,
            super::core::TransferDirection::Upload,
            chunk_index,
            bytes,
            now_b,
        ) {
            Ok(outcome) => outcome,
            Err(error) => {
                // A rejected chunk is a protocol violation for this transfer
                // (size, order, conflict or digest). Fail closed: drop both the
                // staging target and the accounting entry so nothing lingers.
                self.registry.abort(&handle);
                if state.active.contains_key(handle_id) {
                    self.release_staging(&mut state, handle_id);
                }
                return Err(error.code.to_owned());
            }
        };
        if outcome.duplicate {
            return Ok(UploadProgress::Duplicate {
                received_bytes: outcome.received_bytes,
            });
        }
        if let Err(error) = self.staging.accept_chunk(handle_id, chunk_index, bytes) {
            self.registry.abort(&handle);
            self.release_staging(&mut state, handle_id);
            return Err(error);
        }
        match outcome.completed {
            None => Ok(UploadProgress::Accepted {
                received_bytes: outcome.received_bytes,
                next_chunk: outcome.next_chunk,
            }),
            Some(completed) => {
                let sha256 = completed.sha256.clone();
                let bytes = completed.bytes;
                let path = match self.staging.commit(&completed) {
                    Ok(path) if path.is_absolute() => path,
                    Ok(_) => {
                        self.release_staging(&mut state, handle_id);
                        return Err("transfer_staging_failed".to_owned());
                    }
                    Err(error) => {
                        self.release_staging(&mut state, handle_id);
                        return Err(error);
                    }
                };
                let Some(read_owner) = self
                    .staging
                    .published_read_owner(handle_id)
                    .filter(|owner| owner.path() == path)
                else {
                    self.release_staging(&mut state, handle_id);
                    return Err("transfer_staging_failed".to_owned());
                };
                state.active.remove(handle_id);
                state.completed.insert(
                    handle_id.to_owned(),
                    CompletedReceipt {
                        binding: completed.context.binding,
                        descriptor: PublishedAttachment {
                            handle_id: handle_id.to_owned(),
                            path,
                            display_name: completed.metadata.file_name,
                            mime_type: completed.metadata.mime_type,
                            bytes,
                            sha256: sha256.clone(),
                            read_owner: Some(read_owner),
                        },
                        completed_at_ms: now_b,
                        consumed: false,
                    },
                );
                Ok(UploadProgress::Completed { sha256, bytes })
            }
        }
    }

    /// Validate a bearer handle against the *current* authenticated B lease,
    /// authoritative B session and task. The caller holds its lifecycle gate
    /// across this check and the action so a lease switch cannot intervene.
    pub(crate) fn require_upload_binding(
        &self,
        handle_id: &str,
        live: &LeaseContext,
        session_id: &str,
        task_id: &str,
    ) -> std::result::Result<(), String> {
        let handle = Self::handle(handle_id)?;
        let state = self.state.lock();
        if let Some(receipt) = state.completed.get(handle_id) {
            if receipt.consumed || !binding_matches(&receipt.binding, live, session_id, task_id) {
                return Err("transfer_binding_stale".to_owned());
            }
            return Ok(());
        }
        self.registry
            .require_upload_binding(&handle, live, session_id, task_id)
            .map_err(|error| error.code.to_owned())
    }

    /// Single-use admission receipt. All IDs are checked before any are
    /// consumed; on failure the caller cannot partially install attachments.
    /// The published files remain owned here until task/connection cleanup.
    pub(crate) fn consume_completed(
        &self,
        handle_ids: &[String],
        live: &LeaseContext,
        session_id: &str,
        task_id: &str,
    ) -> std::result::Result<Vec<PublishedAttachment>, String> {
        let mut state = self.state.lock();
        if handle_ids.len() > self.registry.limits().max_transfers_per_task
            || handle_ids.iter().collect::<HashSet<_>>().len() != handle_ids.len()
        {
            return Err("transfer_attachment_ids_invalid".to_owned());
        }
        let mut descriptors = Vec::with_capacity(handle_ids.len());
        for handle_id in handle_ids {
            let receipt = state
                .completed
                .get(handle_id)
                .ok_or_else(|| "transfer_unknown".to_owned())?;
            if receipt.consumed || !binding_matches(&receipt.binding, live, session_id, task_id) {
                return Err("transfer_binding_stale".to_owned());
            }
            descriptors.push(receipt.descriptor.clone());
        }
        // All receipts and output descriptors are validated before mutation.
        // Iterate the owned entries under the same lock without a second lookup.
        for (handle_id, receipt) in &mut state.completed {
            if handle_ids.contains(handle_id) {
                receipt.consumed = true;
            }
        }
        Ok(descriptors)
    }

    pub(crate) fn abort(&self, handle_id: &str) -> bool {
        let Ok(handle) = Self::handle(handle_id) else {
            return false;
        };
        let mut state = self.state.lock();
        let known = state.completed.contains_key(handle_id) || state.active.contains_key(handle_id);
        let live = self.registry.abort(&handle);
        if known {
            self.release_staging(&mut state, handle_id);
        }
        known || live
    }

    pub(crate) fn cleanup_for_task(&self, task_id: &str) -> usize {
        self.cleanup_where(|binding| binding.task_id == task_id)
    }

    pub(crate) fn cleanup_for_connection(&self, connection_id: &str) -> usize {
        self.cleanup_where(|binding| binding.connection_id == connection_id)
    }

    /// DEV-17 审核修复：未完成/断线遗留的上传必须在有界时间内清理
    /// （RC-F-003）。过期句柄同时释放预算并删除 staging 目录。
    pub(crate) fn expire(&self, now_b: u64) -> usize {
        let mut state = self.state.lock();
        let expired = self.registry.expire(now_b);
        for handle_id in &expired {
            self.release_staging(&mut state, handle_id);
        }
        let ttl = self.registry.limits().ttl.as_millis() as u64;
        let completed: Vec<String> = state
            .completed
            .iter()
            .filter(|(_, receipt)| {
                !receipt.consumed && now_b.saturating_sub(receipt.completed_at_ms) >= ttl
            })
            .map(|(id, _)| id.clone())
            .collect();
        for handle_id in &completed {
            self.release_staging(&mut state, handle_id);
        }
        let retry: Vec<String> = state.pending_cleanup.iter().cloned().collect();
        for handle_id in retry {
            self.release_staging(&mut state, &handle_id);
        }
        expired.len() + completed.len()
    }

    fn cleanup_where(&self, predicate: impl Fn(&TransferBinding) -> bool) -> usize {
        let mut state = self.state.lock();
        let active: Vec<String> = state
            .active
            .iter()
            .filter(|(_, context)| predicate(&context.binding))
            .map(|(id, _)| id.clone())
            .collect();
        let completed: Vec<String> = state
            .completed
            .iter()
            .filter(|(_, receipt)| predicate(&receipt.binding))
            .map(|(id, _)| id.clone())
            .collect();
        for id in &active {
            if let Ok(handle) = Self::handle(id) {
                self.registry.abort(&handle);
            }
            self.release_staging(&mut state, id);
        }
        for id in &completed {
            self.release_staging(&mut state, id);
        }
        active.len() + completed.len()
    }

    fn release_staging(&self, state: &mut UploadState, handle_id: &str) {
        // Drop the receipt's own Arc before checking for executing owners.
        // Failed cleanup retains the stager authority and retry index only;
        // a revoked receipt must not remain consumable.
        state.active.remove(handle_id);
        state.completed.remove(handle_id);
        if self.staging.abort(handle_id).is_ok() {
            state.pending_cleanup.remove(handle_id);
        } else {
            state.pending_cleanup.insert(handle_id.to_owned());
        }
    }

    fn handle(handle_id: &str) -> std::result::Result<super::core::TransferHandle, String> {
        TransferHandle::from_id(handle_id).ok_or_else(|| "transfer_unknown".to_owned())
    }
}

fn binding_matches(
    binding: &TransferBinding,
    live: &LeaseContext,
    session_id: &str,
    task_id: &str,
) -> bool {
    binding.connection_id == live.connection_id
        && binding.controller_lease_id == live.controller_lease_id
        && binding.lease_epoch == live.lease_epoch
        && binding.relay_epoch == live.relay_epoch
        && binding.remote_control_generation == live.remote_control_generation
        && binding.session_id == session_id
        && binding.task_id == task_id
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use crate::features::remote_control::controller::transfer::{
        TransferDirection, TransferLimits, TransferRegistry,
    };
    use std::sync::Mutex;

    #[derive(Default)]
    struct RecordingStaging {
        calls: Mutex<Vec<String>>,
        fail_on_chunk: bool,
    }

    #[derive(Debug)]
    struct RecordingReadOwner(PathBuf);

    impl crate::features::files::file_ingest::AttachmentReadOwner for RecordingReadOwner {
        fn path(&self) -> &std::path::Path {
            &self.0
        }
        fn read_bounded(&self, _maximum_bytes: u64) -> std::io::Result<Vec<u8>> {
            Err(std::io::Error::new(
                std::io::ErrorKind::Unsupported,
                "recording port has no payload",
            ))
        }
        fn request_cleanup(&self) {}
    }

    impl UploadStaging for RecordingStaging {
        fn begin(
            &self,
            handle_id: &str,
            _context: &TransferContext,
            metadata: &TransferMetadata,
        ) -> std::result::Result<(), String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("begin:{handle_id}"));
            assert!(!metadata.file_name.contains(['/', '\\']));
            Ok(())
        }

        fn accept_chunk(
            &self,
            _handle_id: &str,
            chunk_index: u64,
            bytes: &[u8],
        ) -> std::result::Result<(), String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("chunk:{chunk_index}:{}", bytes.len()));
            if self.fail_on_chunk {
                Err("staging_write_failed".to_owned())
            } else {
                Ok(())
            }
        }

        fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<PathBuf, String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("commit:{}", transfer.sha256));
            Ok(std::env::temp_dir().join(&transfer.handle_id))
        }

        fn published_read_owner(
            &self,
            handle_id: &str,
        ) -> Option<Arc<dyn crate::features::files::file_ingest::AttachmentReadOwner>> {
            Some(Arc::new(RecordingReadOwner(
                std::env::temp_dir().join(handle_id),
            )))
        }

        fn abort(&self, handle_id: &str) -> std::result::Result<(), String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("abort:{handle_id}"));
            Ok(())
        }
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

    fn metadata_for(bytes: &[u8]) -> TransferMetadata {
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

    fn uploads(staging: Arc<RecordingStaging>, limits: TransferLimits) -> AttachmentUploads {
        AttachmentUploads::new(Arc::new(TransferRegistry::new(limits).unwrap()), staging)
    }

    fn upload_context() -> TransferContext {
        TransferContext::new(
            &lease(),
            "session-0001",
            "task-0001",
            TransferDirection::Upload,
        )
        .unwrap()
    }

    #[test]
    fn remote_admission_source_consumes_a_completed_receipt_exactly_once() {
        use crate::features::sessions::application::submit_controlled::resolve_controlled_attachments;
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(staging, TransferLimits::default());
        let data = b"remote attachment";
        let begin = uploads
            .begin(upload_context(), metadata_for(data), 1_000)
            .unwrap();
        uploads
            .accept_chunk(&begin.handle_id, 0, data, 1_100)
            .unwrap();
        let ids = vec![begin.handle_id];
        // Reproduces the old source fold: it must refuse, never consume.
        assert_eq!(
            resolve_controlled_attachments(&ids, Some(Vec::new()), || {
                panic!("local mismatch must not consume an A upload")
            })
            .unwrap_err(),
            "task_attachments_invalid"
        );
        let attachments = resolve_controlled_attachments(&ids, None, || {
            uploads.consume_completed(&ids, &lease(), "session-0001", "task-0001")
        })
        .unwrap();
        assert_eq!(attachments.len(), 1);
        assert_eq!(attachments[0].handle_id, ids[0]);
        assert_eq!(attachments[0].bytes, data.len() as u64);
        assert_eq!(
            resolve_controlled_attachments(&ids, None, || {
                uploads.consume_completed(&ids, &lease(), "session-0001", "task-0001")
            })
            .unwrap_err(),
            "transfer_binding_stale"
        );
    }

    #[test]
    fn staging_port_receives_ordered_chunks_and_a_verified_commit() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(staging.clone(), TransferLimits::default());
        let data = b"hello upload".to_vec();
        let digest = metadata_for(&data).sha256;
        let begin = uploads
            .begin(upload_context(), metadata_for(&data), 1_000)
            .unwrap();
        let progress = uploads
            .accept_chunk(&begin.handle_id, 0, &data, 1_100)
            .unwrap();
        assert_eq!(
            progress,
            UploadProgress::Completed {
                sha256: digest.clone(),
                bytes: data.len() as u64
            }
        );
        assert_eq!(
            staging.calls.lock().unwrap().as_slice(),
            [
                format!("begin:{}", begin.handle_id),
                format!("chunk:0:{}", data.len()),
                format!("commit:{digest}")
            ]
        );
        // A completed handle is consumed exactly once.
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 1, &data, 1_200)
                .unwrap_err(),
            "transfer_unknown"
        );
        assert!(uploads.abort(&begin.handle_id));
        assert!(!uploads.abort(&begin.handle_id));
    }

    #[test]
    fn duplicate_chunks_do_not_restage_but_a_rejected_chunk_cleans_up() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(
            staging.clone(),
            TransferLimits {
                chunk_bytes: 4,
                ..TransferLimits::default()
            },
        );
        let data = b"abcdefgh".to_vec();
        let begin = uploads
            .begin(upload_context(), metadata_for(&data), 1_000)
            .unwrap();
        uploads
            .accept_chunk(&begin.handle_id, 0, &data[..4], 1_100)
            .unwrap();
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 0, &data[..4], 1_200)
                .unwrap(),
            UploadProgress::Duplicate { received_bytes: 4 }
        );
        // The duplicate chunk was not written twice.
        let calls: Vec<String> = staging.calls.lock().unwrap().clone();
        assert_eq!(
            calls
                .iter()
                .filter(|call| call.starts_with("chunk:0"))
                .count(),
            1
        );
        // An over-sized chunk is rejected and its staging target is dropped.
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 1, &data, 1_300)
                .unwrap_err(),
            "transfer_chunk_size_invalid"
        );
        let calls: Vec<String> = staging.calls.lock().unwrap().clone();
        assert!(
            calls
                .iter()
                .any(|call| call == "abort:ctr-up-00000001-1" || call.starts_with("abort:"))
        );
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 1, &data[4..], 1_400)
                .unwrap_err(),
            "transfer_unknown"
        );
    }

    #[test]
    fn chunk_budget_rejection_aborts_staging_and_releases_accounting() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(staging.clone(), TransferLimits::default());
        let chunk_count = super::super::limits::MAX_TRANSFER_CHUNKS;
        let data = vec![1_u8; chunk_count + 1];
        let begin = uploads
            .begin(upload_context(), metadata_for(&data), 1_000)
            .unwrap();
        for index in 0..chunk_count {
            assert_eq!(
                uploads
                    .accept_chunk(&begin.handle_id, index as u64, &[1], 1_100)
                    .unwrap(),
                UploadProgress::Accepted {
                    received_bytes: (index + 1) as u64,
                    next_chunk: (index + 1) as u64,
                }
            );
        }
        assert_eq!(uploads.registry.active_count(), 1);
        assert_eq!(uploads.state.lock().active.len(), 1);
        assert_eq!(
            uploads.registry.connection_bytes("connection-0001"),
            data.len() as u64
        );
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, chunk_count as u64, &[1], 1_200)
                .unwrap_err(),
            "transfer_chunk_budget"
        );
        assert_eq!(uploads.registry.active_count(), 0);
        assert_eq!(uploads.registry.connection_bytes("connection-0001"), 0);
        {
            let state = uploads.state.lock();
            assert!(state.active.is_empty());
            assert!(state.completed.is_empty());
            assert!(state.pending_cleanup.is_empty());
        }
        {
            let calls = staging.calls.lock().unwrap();
            assert_eq!(calls.len(), chunk_count + 2); // begin, accepted chunks, abort
            assert_eq!(
                calls
                    .iter()
                    .filter(|call| call.starts_with("chunk:"))
                    .count(),
                chunk_count
            );
            assert_eq!(calls.last(), Some(&format!("abort:{}", begin.handle_id)));
            assert!(!calls.iter().any(|call| call.starts_with("commit:")));
        }
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 0, &[1], 1_300)
                .unwrap_err(),
            "transfer_unknown"
        );
    }

    #[test]
    fn impossible_chunk_budget_is_refused_before_staging_allocation() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(
            staging.clone(),
            TransferLimits {
                chunk_bytes: 1,
                ..TransferLimits::default()
            },
        );
        let data = vec![1_u8; super::super::limits::MAX_TRANSFER_CHUNKS + 1];
        assert_eq!(
            uploads
                .begin(upload_context(), metadata_for(&data), 1_000)
                .unwrap_err(),
            "transfer_chunk_budget"
        );
        assert!(staging.calls.lock().unwrap().is_empty());
        assert_eq!(uploads.registry.active_count(), 0);
        assert_eq!(uploads.registry.connection_bytes("connection-0001"), 0);
        {
            let state = uploads.state.lock();
            assert!(state.active.is_empty());
            assert!(state.completed.is_empty());
            assert!(state.pending_cleanup.is_empty());
        }
    }

    #[test]
    fn staging_failure_never_leaves_a_live_transfer() {
        let staging = Arc::new(RecordingStaging {
            calls: Mutex::new(Vec::new()),
            fail_on_chunk: true,
        });
        let uploads = uploads(staging, TransferLimits::default());
        let data = b"abcdefgh".to_vec();
        let begin = uploads
            .begin(upload_context(), metadata_for(&data), 1_000)
            .unwrap();
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 0, &data, 1_100)
                .unwrap_err(),
            "staging_write_failed"
        );
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 0, &data, 1_200)
                .unwrap_err(),
            "transfer_unknown"
        );
        assert!(uploads.registry.connection_bytes("connection-0001") == 0);
    }

    #[test]
    fn stale_uploads_expire_and_release_their_staging_and_budget() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(
            staging.clone(),
            TransferLimits {
                ttl: std::time::Duration::from_millis(500),
                ..TransferLimits::default()
            },
        );
        let data = b"abcdefgh".to_vec();
        let begin = uploads
            .begin(upload_context(), metadata_for(&data), 1_000)
            .unwrap();
        uploads
            .accept_chunk(&begin.handle_id, 0, &data[..4], 1_100)
            .unwrap();
        assert_eq!(
            uploads.registry.connection_bytes("connection-0001"),
            data.len() as u64
        );
        // Still inside the TTL.
        assert_eq!(uploads.expire(1_400), 0);
        // Past the TTL: accounting released and staging aborted, exactly once.
        assert_eq!(uploads.expire(1_600), 1);
        assert_eq!(uploads.registry.connection_bytes("connection-0001"), 0);
        assert!(
            staging
                .calls
                .lock()
                .unwrap()
                .iter()
                .any(|call| call.starts_with("abort:"))
        );
        assert_eq!(uploads.expire(1_700), 0);
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 0, &data[..4], 1_800)
                .unwrap_err(),
            "transfer_unknown"
        );
    }

    #[test]
    fn cleanup_by_task_and_connection_releases_every_transfer() {
        let uploads = uploads(
            Arc::new(RecordingStaging::default()),
            TransferLimits::default(),
        );
        let data = b"abcdefgh".to_vec();
        uploads
            .begin(upload_context(), metadata_for(&data), 1_000)
            .unwrap();
        assert_eq!(uploads.cleanup_for_task("task-0001"), 1);
        assert_eq!(uploads.cleanup_for_task("task-0001"), 0);
        uploads
            .begin(upload_context(), metadata_for(&data), 1_100)
            .unwrap();
        assert_eq!(uploads.cleanup_for_connection("connection-0001"), 1);
        assert_eq!(uploads.cleanup_for_connection("connection-0001"), 0);
    }

    #[test]
    fn download_handles_can_never_be_uploaded_into() {
        let uploads = uploads(
            Arc::new(RecordingStaging::default()),
            TransferLimits::default(),
        );
        let data = b"abcdefgh".to_vec();
        // A download ID is rejected before touching the registry, so a
        // cross-direction replay cannot stage an artifact as an attachment.
        assert_eq!(
            uploads
                .accept_chunk("ctr-dn-00000001-1", 0, &data, 1_000)
                .unwrap_err(),
            "transfer_unknown"
        );
        assert_eq!(
            uploads
                .accept_chunk("webup_00000001", 0, &data, 1_000)
                .unwrap_err(),
            "transfer_unknown"
        );
    }

    #[test]
    fn handle_requires_exact_live_lease_session_and_task_before_and_after_completion() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(staging, TransferLimits::default());
        let data = b"hello";
        let begin = uploads
            .begin(upload_context(), metadata_for(data), 1_000)
            .unwrap();
        let valid = || {
            uploads.require_upload_binding(&begin.handle_id, &lease(), "session-0001", "task-0001")
        };
        assert!(valid().is_ok());
        assert_eq!(
            uploads
                .require_upload_binding(&begin.handle_id, &lease(), "other-session", "task-0001")
                .unwrap_err(),
            "transfer_binding_stale"
        );
        assert_eq!(
            uploads
                .require_upload_binding(&begin.handle_id, &lease(), "session-0001", "other-task")
                .unwrap_err(),
            "transfer_binding_stale"
        );
        let mut stale = lease();
        stale.relay_epoch = "relay-epoch-0002".into();
        assert_eq!(
            uploads
                .require_upload_binding(&begin.handle_id, &stale, "session-0001", "task-0001")
                .unwrap_err(),
            "transfer_binding_stale"
        );
        uploads
            .accept_chunk(&begin.handle_id, 0, data, 1_100)
            .unwrap();
        assert!(valid().is_ok());
        assert_eq!(
            uploads
                .require_upload_binding(&begin.handle_id, &stale, "session-0001", "task-0001")
                .unwrap_err(),
            "transfer_binding_stale"
        );
        assert_eq!(
            uploads
                .accept_chunk(&begin.handle_id, 1, data, 1_200)
                .unwrap_err(),
            "transfer_unknown"
        );
        assert!(
            valid().is_ok(),
            "a duplicate completion cannot delete the published receipt"
        );
    }

    #[test]
    fn complete_receipt_is_single_use_atomic_and_cleanup_owns_published_file() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(staging.clone(), TransferLimits::default());
        let data = b"one";
        let first = uploads
            .begin(upload_context(), metadata_for(data), 1_000)
            .unwrap();
        uploads
            .accept_chunk(&first.handle_id, 0, data, 1_100)
            .unwrap();
        let second = uploads
            .begin(upload_context(), metadata_for(data), 1_200)
            .unwrap();
        let ids = vec![first.handle_id.clone(), second.handle_id.clone()];
        assert_eq!(
            uploads
                .consume_completed(&ids, &lease(), "session-0001", "task-0001")
                .unwrap_err(),
            "transfer_unknown"
        );
        uploads
            .accept_chunk(&second.handle_id, 0, data, 1_300)
            .unwrap();
        assert_eq!(
            uploads
                .consume_completed(&ids, &lease(), "session-0001", "wrong-task")
                .unwrap_err(),
            "transfer_binding_stale"
        );
        let descriptors = uploads
            .consume_completed(&ids, &lease(), "session-0001", "task-0001")
            .unwrap();
        assert_eq!(
            descriptors
                .iter()
                .map(|item| item.handle_id.as_str())
                .collect::<Vec<_>>(),
            ids.iter().map(String::as_str).collect::<Vec<_>>()
        );
        assert_eq!(descriptors[0].display_name, "report.txt");
        assert_eq!(descriptors[0].mime_type, "text/plain");
        assert_eq!(descriptors[0].bytes, data.len() as u64);
        assert!(descriptors[0].path.is_absolute());
        assert_eq!(
            uploads
                .consume_completed(&ids, &lease(), "session-0001", "task-0001")
                .unwrap_err(),
            "transfer_binding_stale"
        );
        assert_eq!(uploads.cleanup_for_task("task-0001"), 2);
        assert_eq!(uploads.cleanup_for_task("task-0001"), 0);
        let calls = staging.calls.lock().unwrap();
        assert_eq!(
            calls
                .iter()
                .filter(|call| call.as_str().starts_with("abort:"))
                .count(),
            2
        );
    }

    #[test]
    fn completed_receipt_expires_if_unconsumed_but_not_during_execution() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(
            staging,
            TransferLimits {
                ttl: std::time::Duration::from_millis(500),
                ..TransferLimits::default()
            },
        );
        let data = b"one";
        let first = uploads
            .begin(upload_context(), metadata_for(data), 1_000)
            .unwrap();
        uploads
            .accept_chunk(&first.handle_id, 0, data, 1_100)
            .unwrap();
        assert_eq!(uploads.expire(1_601), 1);
        assert_eq!(
            uploads
                .consume_completed(&[first.handle_id], &lease(), "session-0001", "task-0001")
                .unwrap_err(),
            "transfer_unknown"
        );
        let second = uploads
            .begin(upload_context(), metadata_for(data), 2_000)
            .unwrap();
        uploads
            .accept_chunk(&second.handle_id, 0, data, 2_100)
            .unwrap();
        uploads
            .consume_completed(
                std::slice::from_ref(&second.handle_id),
                &lease(),
                "session-0001",
                "task-0001",
            )
            .unwrap();
        assert_eq!(uploads.expire(3_000), 0);
        assert_eq!(uploads.cleanup_for_connection("connection-0001"), 1);
    }
}
