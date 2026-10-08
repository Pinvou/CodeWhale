//! A→B attachment upload extension slot (implemented by DEV-17).
//!
//! The shared core owns ordering, budgets and digests. DEV-17 only implements
//! this port, which stages the already-accounted bytes into the B session
//! isolation directory. It receives no host path and no file name from A beyond
//! the bounded, slash-free metadata name.

use super::core::{CompletedTransfer, TransferContext, TransferHandle, TransferMetadata};

pub(crate) trait UploadStaging: Send + Sync {
    /// Allocates the staging target for the registry-issued upload handle.
    /// Callers must release registry accounting and abort this same staging
    /// target if allocation fails; host paths never become transfer identities.
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use crate::features::remote_control::controller::transfer::{
        TransferDirection, TransferRegistry,
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
            self.targets.lock().unwrap().remove(handle_id);
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
        let (handle, _) = registry
            .begin(context.clone(), metadata.clone(), 1_000)
            .unwrap();
        staging.begin(&handle, &context, &metadata).unwrap();
        let outcome = registry
            .accept_chunk(&handle, TransferDirection::Upload, 0, &data, 1_100)
            .unwrap();
        staging.accept_chunk(&handle, 0, &data).unwrap();
        let completed = outcome.completed.unwrap();
        staging.commit(&completed).unwrap();
        assert_eq!(completed.sha256, digest);
        assert_eq!(
            staging.calls.lock().unwrap().as_slice(),
            [
                format!("begin:{}", handle.id()),
                format!("chunk:{}:0:{}", handle.id(), data.len()),
                format!("commit:{}:{digest}", handle.id())
            ]
        );
        staging.abort(handle.id()).unwrap();
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
