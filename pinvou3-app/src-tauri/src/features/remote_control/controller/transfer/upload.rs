//! A→B attachment upload extension slot (implemented by DEV-17).
//!
//! The shared core owns ordering, budgets and digests. DEV-17 only implements
//! this port, which stages the already-accounted bytes into the B session
//! isolation directory. It receives no host path and no file name from A beyond
//! the bounded, slash-free metadata name.

use crate::features::remote_control::controller::controlled::{
    ControlledCleanupScope, ControlledCleanupSink,
};
use base64::Engine as _;
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
    pending_cleanup: HashMap<String, TransferBinding>,
    // Only selected, completed receipts belong to an admitted execution.
    // An unclaimed pre-upload with a reused task ID is not that execution.
    admission_owners: HashMap<String, u64>,
}

const MAX_COMPLETED_RECEIPTS: usize = 64;

pub(crate) trait UploadStaging: Send + Sync {
    /// Allocates the staging target for the registry-issued upload handle.
    /// Allocation failures must release registry accounting and abort this
    /// same target; host paths never become transfer identities.
    fn begin(
        &self,
        handle: &TransferHandle,
        context: &TransferContext,
        metadata: &TransferMetadata,
    ) -> std::result::Result<(), String>;

    /// Appends one nonduplicate, core-accepted chunk to this exact handle.
    fn accept_chunk(
        &self,
        handle: &TransferHandle,
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

    /// Drops only the named staging target. Must be idempotent.
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

/// Validate encoded and claimed sizes before the base64 decoder allocates.
pub(crate) fn decode_upload_chunk(
    encoded: &str,
    claimed_bytes: u64,
    limit: usize,
) -> std::result::Result<Vec<u8>, String> {
    if claimed_bytes == 0 || claimed_bytes > limit as u64 || encoded.len() > limit.div_ceil(3) * 4 {
        return Err("transfer_chunk_size_invalid".into());
    }
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(encoded.as_bytes())
        .map_err(|_| "transfer_chunk_invalid".to_owned())?;
    if bytes.len() as u64 != claimed_bytes {
        return Err("transfer_chunk_invalid".into());
    }
    Ok(bytes)
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
    pub(crate) fn chunk_bytes(&self) -> usize {
        self.registry.limits().chunk_bytes
    }
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
        if state.active.len() + state.pending_cleanup.len() >= limits.max_in_flight {
            return Err("transfer_staging_capacity".into());
        }
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
        if let Err(error) = self.staging.begin(&handle, &context, &metadata) {
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
        if !state.active.contains_key(handle_id) || state.pending_cleanup.contains_key(handle_id) {
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
        if let Err(error) = self.staging.accept_chunk(&handle, chunk_index, bytes) {
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
    #[cfg(test)]
    pub(crate) fn consume_completed(
        &self,
        handle_ids: &[String],
        live: &LeaseContext,
        session_id: &str,
        task_id: &str,
    ) -> Result<Vec<PublishedAttachment>, String> {
        self.consume_owned(handle_ids, live, session_id, task_id, None)
    }

    pub(crate) fn consume_for_admission(
        &self,
        handle_ids: &[String],
        live: &LeaseContext,
        session_id: &str,
        task_id: &str,
        sequence: u64,
    ) -> Result<Vec<PublishedAttachment>, String> {
        self.consume_owned(handle_ids, live, session_id, task_id, Some(sequence))
    }

    fn consume_owned(
        &self,
        handle_ids: &[String],
        live: &LeaseContext,
        session_id: &str,
        task_id: &str,
        sequence: Option<u64>,
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
            if receipt.consumed
                || !binding_matches(&receipt.binding, live, session_id, task_id)
                || sequence.is_some_and(|sequence| {
                    state.admission_owners.get(handle_id) != Some(&sequence)
                })
            {
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
        self.cleanup_where(self.registry.take_for_task(task_id), |binding| {
            binding.task_id == task_id
        })
    }

    pub(crate) fn claim_for_admission(
        &self,
        handle_ids: &[String],
        live: &LeaseContext,
        session_id: &str,
        task_id: &str,
        sequence: u64,
    ) -> Result<(), String> {
        let mut state = self.state.lock();
        if handle_ids.len() > self.registry.limits().max_transfers_per_task
            || handle_ids.iter().collect::<HashSet<_>>().len() != handle_ids.len()
        {
            return Err("transfer_attachment_ids_invalid".into());
        }
        for id in handle_ids {
            let receipt = state.completed.get(id).ok_or("transfer_unknown")?;
            if receipt.consumed
                || !binding_matches(&receipt.binding, live, session_id, task_id)
                || state.admission_owners.contains_key(id)
            {
                return Err("transfer_binding_stale".into());
            }
        }
        for id in handle_ids {
            state.admission_owners.insert(id.clone(), sequence);
        }
        Ok(())
    }

    pub(crate) fn cleanup_for_admission(&self, sequence: u64) -> usize {
        let mut state = self.state.lock();
        let ids: Vec<_> = state
            .admission_owners
            .iter()
            .filter(|(_, owner)| **owner == sequence)
            .map(|(id, _)| id.clone())
            .collect();
        for id in &ids {
            // Handles are registry-issued immutable instance IDs, not task IDs.
            if let Ok(handle) = Self::handle(id) {
                self.registry.abort(&handle);
            }
            self.release_staging(&mut state, id);
        }
        ids.len()
    }

    pub(crate) fn cleanup_for_connection(&self, connection_id: &str) -> usize {
        self.cleanup_where(
            self.registry.take_for_connection(connection_id),
            |binding| binding.connection_id == connection_id,
        )
    }

    pub(crate) fn cleanup_for_generation(&self, generation: &str) -> usize {
        self.cleanup_where(self.registry.take_for_generation(generation), |binding| {
            binding.remote_control_generation == generation
        })
    }

    pub(crate) fn verify_binding(
        &self,
        handle_id: &str,
        live: &LeaseContext,
    ) -> Result<(), String> {
        let handle = Self::handle(handle_id)?;
        let state = self.state.lock();
        if let Some(receipt) = state.completed.get(handle_id) {
            return if binding_matches(
                &receipt.binding,
                live,
                &receipt.binding.session_id,
                &receipt.binding.task_id,
            ) {
                Ok(())
            } else {
                Err("transfer_binding_stale".into())
            };
        }
        if !state.active.contains_key(handle_id) {
            return Err("transfer_unknown".into());
        }
        self.registry
            .verify_binding(&handle, live)
            .map_err(|error| error.code.to_owned())
    }

    /// One periodic owner drains typed revocations from both directions, and
    /// retries exact failed upload cleanups without discarding their binding.
    pub(crate) fn expire(&self, now_b: u64) -> usize {
        let mut state = self.state.lock();
        let revoked = self.registry.take_expired(now_b);
        for entry in &revoked {
            if state
                .active
                .get(entry.handle.id())
                .is_some_and(|context| context == &entry.context)
            {
                self.release_staging(&mut state, entry.handle.id());
            }
        }
        let ttl = self.registry.limits().ttl.as_millis() as u64;
        let completed: Vec<_> = state
            .completed
            .iter()
            .filter(|(_, receipt)| {
                !receipt.consumed && now_b.saturating_sub(receipt.completed_at_ms) >= ttl
            })
            .map(|(id, _)| id.clone())
            .collect();
        for id in &completed {
            self.release_staging(&mut state, id);
        }
        let retry: Vec<_> = state.pending_cleanup.keys().cloned().collect();
        for id in retry {
            self.release_staging(&mut state, &id);
        }
        revoked.len() + completed.len()
    }

    pub(crate) fn reap(&self, now_b: u64) -> usize {
        self.expire(now_b)
    }

    pub(crate) fn start_reaper(
        owner: &Arc<Self>,
        clock: Arc<dyn Fn() -> u64 + Send + Sync>,
    ) -> tokio::task::JoinHandle<()> {
        let weak = Arc::downgrade(owner);
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(1));
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                interval.tick().await;
                let Some(owner) = weak.upgrade() else {
                    break;
                };
                let now = clock();
                if tokio::task::spawn_blocking(move || owner.reap(now))
                    .await
                    .is_err()
                {
                    eprintln!("[remote-control] transfer_reaper_failed");
                }
            }
        })
    }

    fn cleanup_where(
        &self,
        revoked: Vec<super::core::RevokedTransfer>,
        predicate: impl Fn(&TransferBinding) -> bool,
    ) -> usize {
        let mut state = self.state.lock();
        for entry in &revoked {
            if state
                .active
                .get(entry.handle.id())
                .is_some_and(|context| context == &entry.context)
            {
                self.release_staging(&mut state, entry.handle.id());
            }
        }
        let completed: Vec<_> = state
            .completed
            .iter()
            .filter(|(_, receipt)| predicate(&receipt.binding))
            .map(|(id, _)| id.clone())
            .collect();
        let pending: Vec<_> = state
            .pending_cleanup
            .iter()
            .filter(|(_, binding)| predicate(binding))
            .map(|(id, _)| id.clone())
            .collect();
        for id in &completed {
            self.release_staging(&mut state, id);
        }
        for id in pending {
            self.release_staging(&mut state, &id);
        }
        revoked.len() + completed.len()
    }

    fn release_staging(&self, state: &mut UploadState, handle_id: &str) {
        // Revoke consumption first and drop our read-owner Arc before cleanup.
        // A failed exact cleanup retains its binding and occupies a bounded slot.
        let binding = state
            .active
            .remove(handle_id)
            .map(|context| context.binding)
            .or_else(|| {
                state
                    .completed
                    .remove(handle_id)
                    .map(|receipt| receipt.binding)
            })
            .or_else(|| state.pending_cleanup.get(handle_id).cloned());
        let Some(binding) = binding else {
            return;
        };
        if self.staging.abort(handle_id).is_ok() {
            state.pending_cleanup.remove(handle_id);
            state.admission_owners.remove(handle_id);
        } else {
            state.pending_cleanup.insert(handle_id.to_owned(), binding);
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

impl ControlledCleanupSink for AttachmentUploads {
    fn cleanup(&self, generation: &str, _scope: ControlledCleanupScope) {
        self.cleanup_for_generation(generation);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use crate::features::remote_control::controller::transfer::{
        TransferDirection, TransferLimits, TransferRegistry,
    };
    use std::collections::HashMap;
    use std::sync::Mutex;

    struct Target {
        context: TransferContext,
        bytes: Vec<u8>,
        next_chunk: u64,
    }

    #[derive(Default)]
    struct RecordingStaging {
        calls: Mutex<Vec<String>>,
        targets: Mutex<HashMap<String, Target>>,
        fail_allocation: bool,
        fail_on_chunk: bool,
        fail_abort: std::sync::atomic::AtomicBool,
        aborted: tokio::sync::Notify,
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
            handle: &TransferHandle,
            context: &TransferContext,
            metadata: &TransferMetadata,
        ) -> std::result::Result<(), String> {
            if self.fail_allocation {
                return Err("injected_allocation_failure".into());
            }
            if handle.direction() != TransferDirection::Upload
                || context.direction != TransferDirection::Upload
            {
                return Err("upload_direction_invalid".into());
            }
            let mut targets = self.targets.lock().unwrap();
            if targets.contains_key(handle.id()) {
                return Err("upload_target_conflict".into());
            }
            targets.insert(
                handle.id().to_owned(),
                Target {
                    context: context.clone(),
                    bytes: Vec::new(),
                    next_chunk: 0,
                },
            );
            self.calls
                .lock()
                .unwrap()
                .push(format!("begin:{}", handle.id()));
            assert!(!metadata.file_name.contains(['/', '\\']));
            Ok(())
        }

        fn accept_chunk(
            &self,
            handle: &TransferHandle,
            chunk_index: u64,
            bytes: &[u8],
        ) -> std::result::Result<(), String> {
            if self.fail_on_chunk {
                return Err("staging_write_failed".to_owned());
            }
            let mut targets = self.targets.lock().unwrap();
            let target = targets
                .get_mut(handle.id())
                .ok_or("upload_target_unknown")?;
            if handle.direction() != TransferDirection::Upload || chunk_index != target.next_chunk {
                return Err("upload_chunk_invalid".into());
            }
            target.bytes.extend_from_slice(bytes);
            target.next_chunk += 1;
            self.calls.lock().unwrap().push(format!(
                "chunk:{}:{chunk_index}:{}",
                handle.id(),
                bytes.len()
            ));
            Ok(())
        }

        fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<PathBuf, String> {
            let targets = self.targets.lock().unwrap();
            let target = targets
                .get(&transfer.handle_id)
                .ok_or("upload_target_unknown")?;
            if target.context != transfer.context {
                return Err("upload_context_invalid".into());
            }
            drop(targets);
            self.calls
                .lock()
                .unwrap()
                .push(format!("commit:{}:{}", transfer.handle_id, transfer.sha256));
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
            if self.fail_abort.load(std::sync::atomic::Ordering::Relaxed) {
                return Err("injected_cleanup_failure".into());
            }
            self.targets.lock().unwrap().remove(handle_id);
            self.aborted.notify_one();
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
    fn authenticated_attachment_chunks_reject_oversize_before_transfer_side_effects() {
        use crate::features::remote_control::controller::protocol::{
            MAX_TRANSFER_CHUNK_BYTES, RuntimeValidationContext, SenderRole,
            validate_controller_runtime_payload_json,
        };
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(staging.clone(), TransferLimits::default());
        let begun = uploads
            .begin(upload_context(), metadata_for(b"abcd"), 1_000)
            .unwrap();
        let handle = AttachmentUploads::handle(&begun.handle_id).unwrap();
        let before = uploads.registry.progress(&handle).unwrap();
        let runtime = RuntimeValidationContext {
            lease: Some(lease()),
            negotiated_capabilities: vec!["attachment_v1".into(), "task_v1".into()],
        };
        let encoded_limit = MAX_TRANSFER_CHUNK_BYTES.div_ceil(3) * 4;
        for (encoded, claimed, rejected_field) in [
            ("!".repeat(encoded_limit + 1), "4".to_owned(), "chunk_b64"),
            (
                "AAAA".repeat(encoded_limit / 4 + 1),
                "4".to_owned(),
                "chunk_b64",
            ),
            (
                "!".into(),
                (MAX_TRANSFER_CHUNK_BYTES + 1).to_string(),
                "chunk_bytes",
            ),
            ("!".into(), "0".into(), "chunk_bytes"),
            ("YWJjZA==".into(), "3".into(), "chunk_b64"),
        ] {
            let live = lease();
            let raw = serde_json::json!({"v":1,"type":"remote_action_request",
                "connection_id":live.connection_id,"controller_lease_id":live.controller_lease_id,
                "lease_epoch":live.lease_epoch,"relay_epoch":live.relay_epoch,
                "remote_control_generation":live.remote_control_generation,
                "request_id":"request-chunk-1","session_id":"session-0001",
                "operation":"attachment_chunk","body":{"task_id":"task-0001",
                    "transfer_id":begun.handle_id,"chunk_index":"0",
                    "chunk_b64":encoded,"chunk_bytes":claimed}})
            .to_string();
            // Use the actual authenticated payload gate before the same decoder
            // consumed by the native handler. No native/Tauri wiring is mocked.
            let result = validate_controller_runtime_payload_json(
                &raw,
                SenderRole::ControllerA,
                Some(&runtime),
            )
            .map_err(|error| error.field)
            .and_then(|payload| {
                let body = &payload["body"];
                let bytes = decode_upload_chunk(
                    body["chunk_b64"].as_str().unwrap(),
                    body["chunk_bytes"].as_str().unwrap().parse().unwrap(),
                    uploads.chunk_bytes(),
                )?;
                uploads.accept_chunk(&begun.handle_id, 0, &bytes, 1_100)
            });
            assert_eq!(result.unwrap_err(), rejected_field);
            assert_eq!(uploads.registry.progress(&handle).unwrap(), before);
            assert_eq!(
                staging.calls.lock().unwrap().as_slice(),
                [format!("begin:{}", begun.handle_id)]
            );
            assert!(
                staging.targets.lock().unwrap()[&begun.handle_id]
                    .bytes
                    .is_empty()
            );
        }
        uploads
            .require_upload_binding(&begun.handle_id, &lease(), "session-0001", "task-0001")
            .unwrap();
        assert!(matches!(
            uploads
                .accept_chunk(&begun.handle_id, 0, b"abcd", 1_200)
                .unwrap(),
            UploadProgress::Completed { bytes: 4, .. }
        ));
    }

    #[test]
    fn oversized_base64_is_rejected_before_decoding_and_declared_size_is_exact() {
        assert_eq!(
            decode_upload_chunk(&"!".repeat(9), 4, 4).unwrap_err(),
            "transfer_chunk_size_invalid"
        );
        assert_eq!(
            decode_upload_chunk("YWJjZA==", 0, 4).unwrap_err(),
            "transfer_chunk_size_invalid"
        );
        assert_eq!(
            decode_upload_chunk("YWJjZA==", 5, 4).unwrap_err(),
            "transfer_chunk_size_invalid"
        );
        assert_eq!(
            decode_upload_chunk("YWJjZA==", 3, 4).unwrap_err(),
            "transfer_chunk_invalid"
        );
        assert_eq!(decode_upload_chunk("YWJjZA==", 4, 4).unwrap(), b"abcd");
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
                format!("chunk:{}:0:{}", begin.handle_id, data.len()),
                format!("commit:{}:{digest}", begin.handle_id)
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
    fn old_admission_cleanup_preserves_same_task_id_new_preuploads_and_receipts() {
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(staging.clone(), TransferLimits::default());
        let data = b"owned upload";
        let context = || {
            TransferContext::new(
                &lease(),
                "session-0001",
                "task-0001",
                TransferDirection::Upload,
            )
            .unwrap()
        };
        let old = uploads.begin(context(), metadata_for(data), 1).unwrap();
        uploads.accept_chunk(&old.handle_id, 0, data, 2).unwrap();
        uploads
            .claim_for_admission(
                std::slice::from_ref(&old.handle_id),
                &lease(),
                "session-0001",
                "task-0001",
                1,
            )
            .unwrap();
        let active = uploads.begin(context(), metadata_for(data), 3).unwrap();
        let new = uploads.begin(context(), metadata_for(data), 4).unwrap();
        uploads.accept_chunk(&new.handle_id, 0, data, 5).unwrap();
        assert_eq!(uploads.cleanup_for_admission(1), 1);
        uploads
            .require_upload_binding(&active.handle_id, &lease(), "session-0001", "task-0001")
            .unwrap();
        uploads
            .require_upload_binding(&new.handle_id, &lease(), "session-0001", "task-0001")
            .unwrap();
        uploads
            .claim_for_admission(
                std::slice::from_ref(&new.handle_id),
                &lease(),
                "session-0001",
                "task-0001",
                257,
            )
            .unwrap();
        assert_eq!(
            uploads
                .consume_for_admission(
                    std::slice::from_ref(&new.handle_id),
                    &lease(),
                    "session-0001",
                    "task-0001",
                    1
                )
                .unwrap_err(),
            "transfer_binding_stale"
        );
        assert!(!uploads.state.lock().completed[&new.handle_id].consumed);
        assert_eq!(uploads.cleanup_for_admission(1), 0);
        assert!(uploads.state.lock().completed.contains_key(&new.handle_id));
        assert_eq!(uploads.cleanup_for_admission(257), 1);
        uploads
            .require_upload_binding(&active.handle_id, &lease(), "session-0001", "task-0001")
            .unwrap();
        uploads.cleanup_for_connection(&lease().connection_id);
        assert!(uploads.state.lock().admission_owners.is_empty());
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
                .filter(|call| call.starts_with(&format!("chunk:{}:0:", begin.handle_id)))
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
            fail_on_chunk: true,
            ..RecordingStaging::default()
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
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(staging.clone(), TransferLimits::default());
        let data = b"abcdefgh".to_vec();
        uploads
            .begin(upload_context(), metadata_for(&data), 1_000)
            .unwrap();
        assert_eq!(uploads.cleanup_for_task("task-0001"), 1);
        assert!(
            staging.targets.lock().unwrap().is_empty(),
            "task cleanup must remove staging"
        );
        assert_eq!(uploads.cleanup_for_task("task-0001"), 0);
        uploads
            .begin(upload_context(), metadata_for(&data), 1_100)
            .unwrap();
        assert_eq!(uploads.cleanup_for_connection("connection-0001"), 1);
        assert!(
            staging.targets.lock().unwrap().is_empty(),
            "connection cleanup must remove staging"
        );
        assert_eq!(uploads.cleanup_for_connection("connection-0001"), 0);
    }

    #[test]
    fn failed_staging_cleanup_retains_a_bounded_retry_and_does_not_touch_other_owner() {
        use std::sync::atomic::Ordering;
        let staging = Arc::new(RecordingStaging::default());
        let uploads = uploads(
            staging.clone(),
            TransferLimits {
                max_in_flight: 2,
                ..TransferLimits::default()
            },
        );
        let data = b"abcdefgh";
        let first = uploads
            .begin(upload_context(), metadata_for(data), 1_000)
            .unwrap();
        let mut other = upload_context();
        other.binding.connection_id = "connection-other".into();
        other.binding.remote_control_generation = "generation-other".into();
        let second = uploads.begin(other, metadata_for(data), 1_000).unwrap();
        staging.fail_abort.store(true, Ordering::Relaxed);
        assert_eq!(uploads.cleanup_for_generation("generation-0001"), 1);
        assert_eq!(uploads.registry.connection_bytes("connection-0001"), 0);
        assert!(
            uploads
                .state
                .lock()
                .pending_cleanup
                .contains_key(&first.handle_id)
        );
        assert!(
            uploads
                .begin(upload_context(), metadata_for(data), 1_001)
                .is_err(),
            "pending cleanup occupies a bounded target slot"
        );
        assert!(
            staging
                .targets
                .lock()
                .unwrap()
                .contains_key(&second.handle_id)
        );
        staging.fail_abort.store(false, Ordering::Relaxed);
        uploads.reap(1_002);
        assert!(
            !staging
                .targets
                .lock()
                .unwrap()
                .contains_key(&first.handle_id)
        );
        assert!(
            staging
                .targets
                .lock()
                .unwrap()
                .contains_key(&second.handle_id)
        );
        assert_eq!(uploads.state.lock().active.len(), 1);
        assert_eq!(uploads.cleanup_for_generation("generation-0001"), 0);
        assert_eq!(uploads.registry.active_count(), 1);
    }

    #[test]
    fn real_periodic_reaper_reclaims_stalled_upload_without_commands() {
        use std::sync::atomic::{AtomicU64, Ordering};
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            tokio::time::pause();
            let staging = Arc::new(RecordingStaging::default());
            let uploads = Arc::new(uploads(
                staging.clone(),
                TransferLimits {
                    ttl: std::time::Duration::from_millis(500),
                    ..TransferLimits::default()
                },
            ));
            let begin = uploads
                .begin(upload_context(), metadata_for(b"stalled"), 1_000)
                .unwrap();
            let clock = Arc::new(AtomicU64::new(1_000));
            let tick = Arc::new(tokio::sync::Notify::new());
            let worker = AttachmentUploads::start_reaper(&uploads, {
                let clock = clock.clone();
                let tick = tick.clone();
                Arc::new(move || {
                    let now = clock.load(Ordering::Relaxed);
                    tick.notify_one();
                    now
                })
            });
            tick.notified().await; // the initial tick is not yet expired
            clock.store(2_000, Ordering::Relaxed);
            tokio::time::advance(std::time::Duration::from_secs(1)).await;
            staging.aborted.notified().await; // real interval + real cleanup, no command call
            assert!(
                !staging
                    .targets
                    .lock()
                    .unwrap()
                    .contains_key(&begin.handle_id)
            );
            assert_eq!(uploads.registry.active_count(), 0);
            assert_eq!(uploads.registry.connection_bytes("connection-0001"), 0);
            worker.abort();
        });
    }

    #[test]
    fn transfer_ports_reject_every_changed_lease_field_before_chunk_or_abort() {
        use super::super::download::{ArtifactDownloads, ArtifactIndex, MemoryArtifactSource};
        let registry = Arc::new(TransferRegistry::default());
        let uploads =
            AttachmentUploads::new(registry.clone(), Arc::new(RecordingStaging::default()));
        let index = Arc::new(ArtifactIndex::default());
        let data = b"abcdefgh";
        index
            .register(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                "result.txt",
                "text/plain",
                &metadata_for(data).sha256,
                Arc::new(MemoryArtifactSource::new(data.to_vec())),
            )
            .unwrap();
        let downloads = ArtifactDownloads::new(registry.clone(), index);
        let up = uploads
            .begin(upload_context(), metadata_for(data), 1_000)
            .unwrap();
        let down = downloads
            .begin(
                &lease(),
                "session-0001",
                "task-0001",
                "artifact-0001",
                1_000,
            )
            .unwrap();
        for field in 0..5 {
            let mut stale = lease();
            match field {
                0 => stale.connection_id = "foreign".into(),
                1 => stale.controller_lease_id = "foreign".into(),
                2 => stale.lease_epoch = "2".into(),
                3 => stale.relay_epoch = "foreign".into(),
                _ => stale.remote_control_generation = "foreign".into(),
            }
            assert_eq!(
                uploads.verify_binding(&up.handle_id, &stale).unwrap_err(),
                "transfer_binding_stale"
            );
            assert_eq!(
                downloads
                    .verify_binding(&down.handle_id, &stale)
                    .unwrap_err(),
                "transfer_binding_stale"
            );
        }
        assert_eq!(
            registry.active_count(),
            2,
            "stale validation cannot revoke the rightful owner"
        );
        uploads.verify_binding(&up.handle_id, &lease()).unwrap();
        downloads.verify_binding(&down.handle_id, &lease()).unwrap();
        assert_eq!(
            uploads.cleanup_for_task("task-0001"),
            2,
            "one lifecycle owner revokes both directions"
        );
        assert_eq!(registry.active_count(), 0);
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

    #[test]
    fn interleaved_upload_handles_route_chunks_to_distinct_targets() {
        let registry = TransferRegistry::default();
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
            file_name: "notes.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: 4,
            sha256: "a".repeat(64),
        };
        let staging = RecordingStaging::default();
        let (a, _) = registry
            .begin(context.clone(), metadata.clone(), 1_000)
            .unwrap();
        let (b, _) = registry
            .begin(context.clone(), metadata.clone(), 1_000)
            .unwrap();
        staging.begin(&a, &context, &metadata).unwrap();
        staging.begin(&b, &context, &metadata).unwrap();
        for (handle, bytes) in [
            (&a, b"AA".as_slice()),
            (&b, b"BB".as_slice()),
            (&a, b"AA".as_slice()),
        ] {
            let accepted = registry
                .accept_chunk(handle, TransferDirection::Upload, 0, bytes, 1_100)
                .unwrap();
            if !accepted.duplicate {
                staging.accept_chunk(handle, 0, bytes).unwrap();
            }
        }
        assert_eq!(staging.targets.lock().unwrap()[a.id()].bytes, b"AA");
        assert_eq!(staging.targets.lock().unwrap()[b.id()].bytes, b"BB");
        let (unknown, _) = registry
            .begin(context.clone(), metadata.clone(), 1_000)
            .unwrap();
        assert!(staging.accept_chunk(&unknown, 0, b"XX").is_err());
        // A failed allocation releases only that registry handle; siblings stay live.
        let failing = RecordingStaging {
            fail_allocation: true,
            ..Default::default()
        };
        assert!(failing.begin(&unknown, &context, &metadata).is_err());
        assert!(registry.abort(&unknown));
        staging.abort(unknown.id()).unwrap();
        assert!(registry.progress(&a).is_ok());
        assert!(registry.progress(&b).is_ok());
        let mut wrong = context.clone();
        wrong.direction = TransferDirection::Download;
        assert!(staging.begin(&unknown, &wrong, &metadata).is_err());
        let completed = CompletedTransfer {
            handle_id: a.id().into(),
            context: wrong,
            metadata: metadata.clone(),
            sha256: "a".repeat(64),
            bytes: 2,
        };
        assert!(staging.commit(&completed).is_err());
        for field in 0..4 {
            let mut wrong = context.clone();
            match field {
                0 => wrong.binding.controller_lease_id = "lease-other".into(),
                1 => wrong.binding.session_id = "session-other".into(),
                2 => wrong.binding.task_id = "task-other".into(),
                _ => wrong.binding.remote_control_generation = "generation-other".into(),
            }
            let completed = CompletedTransfer {
                handle_id: a.id().into(),
                context: wrong,
                metadata: metadata.clone(),
                sha256: "a".repeat(64),
                bytes: 2,
            };
            assert!(staging.commit(&completed).is_err());
        }
        staging.abort(a.id()).unwrap();
        staging.abort(a.id()).unwrap();
        assert!(!staging.targets.lock().unwrap().contains_key(a.id()));
        assert_eq!(staging.targets.lock().unwrap()[b.id()].bytes, b"BB");
    }
}
