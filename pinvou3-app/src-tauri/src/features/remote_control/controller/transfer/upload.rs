//! A→B attachment upload extension slot (implemented by DEV-17).
//!
//! The shared core owns ordering, budgets and digests. DEV-17 only implements
//! this port, which stages the already-accounted bytes into the B session
//! isolation directory. It receives no host path and no file name from A beyond
//! the bounded, slash-free metadata name.

use base64::Engine as _;
use std::collections::HashMap;
use std::sync::Arc;

use crate::features::remote_control::controller::controlled::{
    ControlledCleanupScope, ControlledCleanupSink,
};
use parking_lot::Mutex;

use super::core::{CompletedTransfer, TransferContext, TransferHandle, TransferMetadata};

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
    fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<(), String>;

    /// Drops only the named staging target. Must be idempotent.
    fn abort(&self, handle_id: &str) -> std::result::Result<(), String>;
}

/// Upload owner: binds the shared transfer core to one staging port. The
/// successful handle is consumed exactly once, and every failure path aborts
/// the staging target so no orphan cache survives.
pub(crate) struct AttachmentUploads {
    registry: Arc<super::core::TransferRegistry>,
    staging: Arc<dyn UploadStaging>,
    // Live targets and revoked targets awaiting cleanup share a bounded slot
    // ledger. Failed cleanup never loses its exact handle/context or admits an
    // unbounded succession of new files.
    targets: Mutex<HashMap<String, UploadTarget>>,
}

struct UploadTarget {
    context: TransferContext,
    cleanup_pending: bool,
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
            targets: Mutex::new(HashMap::new()),
        }
    }

    pub(crate) fn begin(
        &self,
        context: TransferContext,
        metadata: TransferMetadata,
        now_b: u64,
    ) -> std::result::Result<UploadBegin, String> {
        if context.direction != super::core::TransferDirection::Upload {
            return Err("transfer_direction_mismatch".to_owned());
        }
        let mut targets = self.targets.lock();
        if targets.len() >= self.registry.limits().max_in_flight {
            return Err("transfer_staging_capacity".into());
        }
        let (handle, progress) = self
            .registry
            .begin(context.clone(), metadata.clone(), now_b)
            .map_err(|error| error.code.to_owned())?;
        targets.insert(
            handle.id().to_owned(),
            UploadTarget {
                context: context.clone(),
                cleanup_pending: false,
            },
        );
        if let Err(error) = self.staging.begin(&handle, &context, &metadata) {
            self.registry.abort(&handle);
            self.abort_target(&mut targets, handle.id());
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
        let mut targets = self.targets.lock();
        if targets
            .get(handle_id)
            .is_none_or(|target| target.cleanup_pending)
        {
            return Err("transfer_unknown".into());
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
                self.abort_target(&mut targets, handle_id);
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
            self.abort_target(&mut targets, handle_id);
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
                if let Err(error) = self.staging.commit(&completed) {
                    self.abort_target(&mut targets, handle_id);
                    return Err(error);
                }
                targets.remove(handle_id);
                Ok(UploadProgress::Completed { sha256, bytes })
            }
        }
    }

    pub(crate) fn abort(&self, handle_id: &str) -> bool {
        let Ok(handle) = Self::handle(handle_id) else {
            return false;
        };
        if handle.direction() != super::core::TransferDirection::Upload {
            return false;
        }
        let mut targets = self.targets.lock();
        let revoked = self.registry.abort(&handle);
        self.abort_target(&mut targets, handle_id);
        revoked
    }

    pub(crate) fn cleanup_for_task(&self, task_id: &str) -> usize {
        let mut targets = self.targets.lock();
        let revoked = self.registry.take_for_task(task_id);
        self.clean_revoked(&mut targets, &revoked);
        self.retry_matching(&mut targets, |target| {
            target.context.binding.task_id == task_id
        });
        revoked.len()
    }

    pub(crate) fn cleanup_for_connection(&self, connection_id: &str) -> usize {
        let mut targets = self.targets.lock();
        let revoked = self.registry.take_for_connection(connection_id);
        self.clean_revoked(&mut targets, &revoked);
        self.retry_matching(&mut targets, |target| {
            target.context.binding.connection_id == connection_id
        });
        revoked.len()
    }

    pub(crate) fn verify_binding(
        &self,
        handle_id: &str,
        live: &super::super::protocol::LeaseContext,
    ) -> std::result::Result<(), String> {
        let handle = Self::handle(handle_id)?;
        if handle.direction() != super::core::TransferDirection::Upload {
            return Err("transfer_direction_mismatch".into());
        }
        self.registry
            .verify_binding(&handle, live)
            .map_err(|error| error.code.to_owned())
    }

    pub(crate) fn cleanup_for_generation(&self, generation: &str) -> usize {
        let mut targets = self.targets.lock();
        let revoked = self.registry.take_for_generation(generation);
        self.clean_revoked(&mut targets, &revoked);
        self.retry_matching(&mut targets, |target| {
            target.context.binding.remote_control_generation == generation
        });
        revoked.len()
    }

    /// Called by the production periodic reaper even when no commands arrive.
    /// Revokes both directions, then retries failed upload cleanup by exact ID.
    pub(crate) fn reap(&self, now_b: u64) -> usize {
        let mut targets = self.targets.lock();
        let revoked = self.registry.take_expired(now_b);
        self.clean_revoked(&mut targets, &revoked);
        self.retry_matching(&mut targets, |_| true);
        revoked.len()
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

    fn clean_revoked(
        &self,
        targets: &mut HashMap<String, UploadTarget>,
        revoked: &[super::core::RevokedTransfer],
    ) {
        for entry in revoked {
            if entry.handle.direction() == super::core::TransferDirection::Upload {
                if let Some(target) = targets.get_mut(entry.handle.id()) {
                    // A mismatch must never let stale revocation erase a new owner.
                    if target.context == entry.context {
                        target.cleanup_pending = true;
                    }
                }
            }
        }
    }

    fn retry_matching(
        &self,
        targets: &mut HashMap<String, UploadTarget>,
        predicate: impl Fn(&UploadTarget) -> bool,
    ) {
        let ids: Vec<_> = targets
            .iter()
            .filter(|(_, target)| target.cleanup_pending && predicate(target))
            .map(|(id, _)| id.clone())
            .collect();
        for id in ids {
            self.abort_target(targets, &id);
        }
    }

    fn abort_target(&self, targets: &mut HashMap<String, UploadTarget>, id: &str) {
        let Some(target) = targets.get_mut(id) else {
            return;
        };
        target.cleanup_pending = true;
        match self.staging.abort(id) {
            Ok(()) => {
                targets.remove(id);
            }
            // Do not log platform error details, which could contain host paths.
            Err(_) => eprintln!("[remote-control] transfer_staging_cleanup_failed: retry retained"),
        }
    }

    fn handle(handle_id: &str) -> std::result::Result<super::core::TransferHandle, String> {
        TransferHandle::from_id(handle_id).ok_or_else(|| "transfer_unknown".to_owned())
    }
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

        fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<(), String> {
            let mut targets = self.targets.lock().unwrap();
            let target = targets
                .get(&transfer.handle_id)
                .ok_or("upload_target_unknown")?;
            if target.context != transfer.context {
                return Err("upload_context_invalid".into());
            }
            targets.remove(&transfer.handle_id);
            self.calls
                .lock()
                .unwrap()
                .push(format!("commit:{}:{}", transfer.handle_id, transfer.sha256));
            Ok(())
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
                .targets
                .lock()
                .get(&first.handle_id)
                .unwrap()
                .cleanup_pending
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
        assert_eq!(uploads.targets.lock().len(), 1);
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
                "session-0001",
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
