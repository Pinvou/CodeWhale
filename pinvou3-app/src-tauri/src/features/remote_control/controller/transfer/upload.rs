//! A→B attachment upload extension slot (implemented by DEV-17).
//!
//! The shared core owns ordering, budgets and digests. DEV-17 only implements
//! this port, which stages the already-accounted bytes into the B session
//! isolation directory. It receives no host path and no file name from A beyond
//! the bounded, slash-free metadata name.

use std::sync::Arc;

use super::core::{CompletedTransfer, TransferContext, TransferHandle, TransferMetadata};

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
    fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<(), String>;

    /// Drops the staging target. Must be idempotent.
    fn abort(&self, handle_id: &str) -> std::result::Result<(), String>;
}

/// Upload owner: binds the shared transfer core to one staging port. The
/// successful handle is consumed exactly once, and every failure path aborts
/// the staging target so no orphan cache survives.
pub(crate) struct AttachmentUploads {
    registry: Arc<super::core::TransferRegistry>,
    staging: Arc<dyn UploadStaging>,
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
        Self { registry, staging }
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
        let (handle, progress) = self
            .registry
            .begin(context.clone(), metadata.clone(), now_b)
            .map_err(|error| error.code.to_owned())?;
        if let Err(error) = self.staging.begin(handle.id(), &context, &metadata) {
            self.registry.abort(&handle);
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
                let _ = self.staging.abort(handle_id);
                self.registry.abort(&handle);
                return Err(error.code.to_owned());
            }
        };
        if outcome.duplicate {
            return Ok(UploadProgress::Duplicate {
                received_bytes: outcome.received_bytes,
            });
        }
        if let Err(error) = self
            .staging
            .accept_chunk(handle_id, chunk_index, bytes)
        {
            let _ = self.staging.abort(handle_id);
            self.registry.abort(&handle);
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
                    let _ = self.staging.abort(handle_id);
                    return Err(error);
                }
                Ok(UploadProgress::Completed { sha256, bytes })
            }
        }
    }

    pub(crate) fn abort(&self, handle_id: &str) -> bool {
        let Ok(handle) = Self::handle(handle_id) else {
            return false;
        };
        let _ = self.staging.abort(handle_id);
        self.registry.abort(&handle)
    }

    pub(crate) fn cleanup_for_task(&self, task_id: &str) -> usize {
        self.registry.cleanup_for_task(task_id)
    }

    pub(crate) fn cleanup_for_connection(&self, connection_id: &str) -> usize {
        self.registry.cleanup_for_connection(connection_id)
    }

    /// DEV-17 审核修复：未完成/断线遗留的上传必须在有界时间内清理
    /// （RC-F-003）。过期句柄同时释放预算并删除 staging 目录。
    pub(crate) fn expire(&self, now_b: u64) -> usize {
        let expired = self.registry.expire(now_b);
        for handle_id in &expired {
            let _ = self.staging.abort(handle_id);
        }
        expired.len()
    }

    fn handle(handle_id: &str) -> std::result::Result<super::core::TransferHandle, String> {
        TransferHandle::from_id(handle_id).ok_or_else(|| "transfer_unknown".to_owned())
    }
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

        fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<(), String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("commit:{}", transfer.sha256));
            Ok(())
        }

        fn abort(&self, handle_id: &str) -> std::result::Result<(), String> {
            self.calls.lock().unwrap().push(format!("abort:{handle_id}"));
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

    fn uploads(
        staging: Arc<RecordingStaging>,
        limits: TransferLimits,
    ) -> AttachmentUploads {
        AttachmentUploads::new(
            Arc::new(TransferRegistry::new(limits).unwrap()),
            staging,
        )
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
            calls.iter().filter(|call| call.starts_with("chunk:0")).count(),
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
        assert!(calls.iter().any(|call| call == "abort:ctr-up-00000001-1"
            || call.starts_with("abort:")));
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
        assert_eq!(uploads.registry.connection_bytes("connection-0001"), data.len() as u64);
        // Still inside the TTL.
        assert_eq!(uploads.expire(1_400), 0);
        // Past the TTL: accounting released and staging aborted, exactly once.
        assert_eq!(uploads.expire(1_600), 1);
        assert_eq!(uploads.registry.connection_bytes("connection-0001"), 0);
        assert!(staging
            .calls
            .lock()
            .unwrap()
            .iter()
            .any(|call| call.starts_with("abort:")));
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
}
