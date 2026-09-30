//! A→B attachment upload extension slot (implemented by DEV-17).
//!
//! The shared core owns ordering, budgets and digests. DEV-17 only implements
//! this port, which stages the already-accounted bytes into the B session
//! isolation directory. It receives no host path and no file name from A beyond
//! the bounded, slash-free metadata name.

use super::core::{CompletedTransfer, TransferContext, TransferMetadata};

pub(crate) trait UploadStaging: Send + Sync {
    /// Allocates the staging target for one transfer.
    fn begin(
        &self,
        context: &TransferContext,
        metadata: &TransferMetadata,
    ) -> std::result::Result<(), String>;

    /// Appends one accepted chunk in order.
    fn accept_chunk(&self, chunk_index: u64, bytes: &[u8]) -> std::result::Result<(), String>;

    /// Publishes the staged file once the core verified the digest.
    fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<(), String>;

    /// Drops the staging target. Must be idempotent.
    fn abort(&self, handle_id: &str) -> std::result::Result<(), String>;
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use crate::features::remote_control::controller::transfer::{
        TransferDirection, TransferRegistry,
    };
    use std::sync::Mutex;

    #[derive(Default)]
    struct RecordingStaging {
        calls: Mutex<Vec<String>>,
    }

    impl UploadStaging for RecordingStaging {
        fn begin(
            &self,
            _context: &TransferContext,
            metadata: &TransferMetadata,
        ) -> std::result::Result<(), String> {
            self.calls.lock().unwrap().push("begin".into());
            assert!(!metadata.file_name.contains(['/', '\\']));
            Ok(())
        }

        fn accept_chunk(&self, chunk_index: u64, bytes: &[u8]) -> std::result::Result<(), String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("chunk:{chunk_index}:{}", bytes.len()));
            Ok(())
        }

        fn commit(&self, transfer: &CompletedTransfer) -> std::result::Result<(), String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("commit:{}", transfer.sha256));
            Ok(())
        }

        fn abort(&self, handle_id: &str) -> std::result::Result<(), String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("abort:{handle_id}"));
            Ok(())
        }
    }

    #[test]
    fn staging_port_receives_ordered_chunks_and_a_verified_commit() {
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
        let data = b"hello upload".to_vec();
        let digest = {
            use sha2::{Digest, Sha256};
            Sha256::digest(&data)
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>()
        };
        let metadata = TransferMetadata {
            file_name: "notes.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: data.len() as u64,
            sha256: digest.clone(),
        };
        let staging = RecordingStaging::default();
        staging.begin(&context, &metadata).unwrap();
        let (handle, _) = registry.begin(context, metadata, 1_000).unwrap();
        let outcome = registry
            .accept_chunk(&handle, TransferDirection::Upload, 0, &data, 1_100)
            .unwrap();
        staging.accept_chunk(0, &data).unwrap();
        let completed = outcome.completed.unwrap();
        staging.commit(&completed).unwrap();
        assert_eq!(completed.sha256, digest);
        assert_eq!(
            staging.calls.lock().unwrap().as_slice(),
            [
                "begin".to_owned(),
                format!("chunk:0:{}", data.len()),
                format!("commit:{digest}")
            ]
        );
        staging.abort(handle.id()).unwrap();
    }
}
