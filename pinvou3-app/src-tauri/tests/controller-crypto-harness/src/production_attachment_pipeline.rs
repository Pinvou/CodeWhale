//! Exercises production staged ingest; assistant wrapper is outside this host.
#[cfg(all(test, windows))]
mod tests {
    use crate::features::files::file_ingest;
    use crate::features::remote_control::controller::protocol::LeaseContext;
    use std::sync::Arc;
    #[test]
    fn rejected_chunk_can_be_reconciled_as_absent_before_next_upload() {
        use crate::crypto::ControllerRole;
        use crate::desktop_remote_action_state::DesktopRemoteActionState;
        use crate::encrypted_lane::EncryptedLane;
        use crate::features::remote_control::controller::transfer::{
            TransferContext, TransferDirection, TransferMetadata, TransferRegistry,
            staging::SessionIsolationStaging, upload::AttachmentUploads,
        };
        use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
        use crate::test_support::{lease_context, paired_session_keys, pairing_fixture};
        use serde_json::json;
        use sha2::{Digest, Sha256};

        let root = tempfile::tempdir().unwrap();
        let resolved_root = root.path().to_owned();
        let staging = Arc::new(SessionIsolationStaging::new(Arc::new(move |_| {
            Ok(resolved_root.clone())
        })));
        let uploads = AttachmentUploads::new(Arc::new(TransferRegistry::default()), staging);
        let fixture = pairing_fixture("relay-key-action-chain");
        let lease = lease_context(&fixture.reservation);
        let (a_keys, b_keys) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let mut lane_a = EncryptedLane::new(ControllerRole::A, &lease, a_keys).unwrap();
        let mut lane_b = EncryptedLane::new(ControllerRole::B, &lease, b_keys).unwrap();
        let capabilities = vec![
            "attachment_v1".to_owned(),
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "task_v1".to_owned(),
        ];
        let mut opened = |mut payload: serde_json::Value| {
            let object = payload.as_object_mut().unwrap();
            object.insert("v".into(), json!(1));
            object.insert("type".into(), json!("remote_action_result"));
            object.insert("connection_id".into(), json!(lease.connection_id));
            object.insert(
                "controller_lease_id".into(),
                json!(lease.controller_lease_id),
            );
            object.insert("lease_epoch".into(), json!(lease.lease_epoch));
            object.insert("relay_epoch".into(), json!(lease.relay_epoch));
            object.insert(
                "remote_control_generation".into(),
                json!(lease.remote_control_generation),
            );
            object.entry("body").or_insert(json!({}));
            let ctx = crate::protocol::RuntimeValidationContext {
                lease: Some(lease.clone()),
                negotiated_capabilities: capabilities.clone(),
            };
            crate::protocol::validate_controller_runtime_payload_json(
                &payload.to_string(),
                crate::protocol::SenderRole::ControlledB,
                Some(&ctx),
            )
            .unwrap();
            let frame = lane_b
                .seal("remote_action_result", &payload, &capabilities)
                .unwrap();
            let (kind, payload) = lane_a.open(&frame, &capabilities).unwrap();
            assert_eq!(kind, "remote_action_result");
            payload
        };
        let live = LiveRemoteContext {
            peer_id: "peer-1".into(),
            resume_boundary_id: "resume-1".into(),
            connection_id: lease.connection_id.clone(),
            controller_lease_id: lease.controller_lease_id.clone(),
            lease_epoch: lease.lease_epoch.clone(),
            relay_epoch: lease.relay_epoch.clone(),
            generation: lease.remote_control_generation.clone(),
            control_connected: true,
            encrypted_channel_connected: true,
        };
        let context =
            TransferContext::new(&lease, "session-1", "task-1", TransferDirection::Upload).unwrap();
        let metadata = TransferMetadata {
            file_name: "test.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: 3,
            sha256: Sha256::digest(b"abc")
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect(),
        };
        let state = DesktopRemoteActionState::default();
        let first = uploads.begin(context.clone(), metadata.clone(), 1).unwrap();
        let other_context =
            TransferContext::new(&lease, "session-1", "other-task", TransferDirection::Upload)
                .unwrap();
        let other = uploads.begin(other_context, metadata.clone(), 1).unwrap();
        let mut receiver = state
            .register(
                "request-begin".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_begin".into(),
                Some("task-1".into()),
                None,
            )
            .unwrap();
        let begin_reply = opened(
            json!({"request_id":"request-begin","session_id":"session-1","operation":"attachment_begin","ok":true,"body":{"transfer_id":first.handle_id,"chunk_bytes":first.chunk_bytes.to_string(),"total_bytes":"3"}}),
        );
        assert!(state.settle_authenticated(&lease, &live, &begin_reply));
        assert!(receiver.try_recv().unwrap().is_ok());
        uploads
            .require_upload_binding(&first.handle_id, &lease, "session-1", "task-1")
            .unwrap();
        let error = uploads
            .accept_chunk(&first.handle_id, 1, b"abc", 2)
            .unwrap_err();
        let mut receiver = state
            .register(
                "request-chunk".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_chunk".into(),
                Some("task-1".into()),
                Some(first.handle_id.clone()),
            )
            .unwrap();
        let chunk_reply = opened(
            json!({"request_id":"request-chunk","session_id":"session-1","operation":"attachment_chunk","ok":false,"error_code":error}),
        );
        assert!(state.settle_authenticated(&lease, &live, &chunk_reply));
        assert!(receiver.try_recv().unwrap().is_err());
        assert!(
            state
                .validate_completed_uploads(
                    "local-1",
                    "session-1",
                    &live,
                    "task-1",
                    std::slice::from_ref(&first.handle_id)
                )
                .is_err()
        );
        let _unknown_reply = state
            .register(
                "unknown-abort".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_abort".into(),
                Some("task-1".into()),
                Some(first.handle_id.clone()),
            )
            .unwrap();
        state.detach_uncertain("unknown-abort");
        assert!(
            state
                .validate_completed_uploads(
                    "local-1",
                    "session-1",
                    &live,
                    "task-1",
                    std::slice::from_ref(&first.handle_id)
                )
                .is_err()
        );
        uploads
            .require_upload_binding(&other.handle_id, &lease, "session-1", "other-task")
            .unwrap();
        let mut late = state
            .register(
                "late-chunk".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_chunk".into(),
                Some("task-1".into()),
                Some(first.handle_id.clone()),
            )
            .unwrap();
        let mut wrong_lease = lease.clone();
        wrong_lease.lease_epoch = "2".into();
        let late_payload = opened(
            json!({"request_id":"late-chunk","session_id":"session-1","operation":"attachment_chunk","ok":true,"body":{"state":"completed","received_bytes":"3"}}),
        );
        assert!(!state.settle_authenticated(&wrong_lease, &live, &late_payload));
        assert!(state.settle_authenticated(&lease, &live, &late_payload));
        assert!(late.try_recv().unwrap().is_err());
        assert!(
            state
                .validate_completed_uploads(
                    "local-1",
                    "session-1",
                    &live,
                    "task-1",
                    std::slice::from_ref(&first.handle_id)
                )
                .is_err()
        );
        let absent = uploads
            .require_upload_binding(&first.handle_id, &lease, "session-1", "task-1")
            .unwrap_err();
        assert_eq!(absent, "transfer_unknown");
        let mut receiver = state
            .register(
                "request-abort".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_abort".into(),
                Some("task-1".into()),
                Some(first.handle_id.clone()),
            )
            .unwrap();
        let abort_reply = opened(
            json!({"request_id":"request-abort","session_id":"session-1","operation":"attachment_abort","ok":false,"error_code":absent}),
        );
        assert!(state.settle_authenticated(&lease, &live, &abort_reply));
        assert_eq!(
            receiver.try_recv().unwrap().unwrap()["already_absent"],
            true
        );
        let second = uploads.begin(context, metadata, 3).unwrap();
        assert_ne!(first.handle_id, second.handle_id);
        let mut second_begin = state
            .register(
                "second-begin".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_begin".into(),
                Some("task-1".into()),
                None,
            )
            .unwrap();
        let reply = opened(
            json!({"request_id":"second-begin","session_id":"session-1","operation":"attachment_begin","ok":true,"body":{"transfer_id":second.handle_id,"chunk_bytes":second.chunk_bytes.to_string(),"total_bytes":"3"}}),
        );
        assert!(state.settle_authenticated(&lease, &live, &reply));
        assert!(second_begin.try_recv().unwrap().is_ok());
        uploads
            .require_upload_binding(&second.handle_id, &lease, "session-1", "task-1")
            .unwrap();
        assert!(matches!(
            uploads
                .accept_chunk(&second.handle_id, 0, b"abc", 4)
                .unwrap(),
            crate::transfer::upload::UploadProgress::Completed { .. }
        ));
        let mut second_chunk = state
            .register(
                "second-chunk".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_chunk".into(),
                Some("task-1".into()),
                Some(second.handle_id.clone()),
            )
            .unwrap();
        let reply = opened(
            json!({"request_id":"second-chunk","session_id":"session-1","operation":"attachment_chunk","ok":true,"body":{"state":"completed","received_bytes":"3"}}),
        );
        assert!(state.settle_authenticated(&lease, &live, &reply));
        assert!(second_chunk.try_recv().unwrap().is_ok());
        state
            .validate_completed_uploads(
                "local-1",
                "session-1",
                &live,
                "task-1",
                std::slice::from_ref(&second.handle_id),
            )
            .unwrap();
        assert!(uploads.abort(&second.handle_id));
        assert!(uploads.abort(&other.handle_id));

        let lost_context =
            TransferContext::new(&lease, "session-1", "lost-task", TransferDirection::Upload)
                .unwrap();
        let lost_metadata = TransferMetadata {
            file_name: "lost.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: 3,
            sha256: Sha256::digest(b"abc")
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect(),
        };
        let lost = uploads.begin(lost_context, lost_metadata, 5).unwrap();
        let mut begin_waiter = state
            .register(
                "lost-begin".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_begin".into(),
                Some("lost-task".into()),
                None,
            )
            .unwrap();
        let reply = opened(
            json!({"request_id":"lost-begin","session_id":"session-1","operation":"attachment_begin","ok":true,"body":{"transfer_id":lost.handle_id,"chunk_bytes":lost.chunk_bytes.to_string(),"total_bytes":"3"}}),
        );
        assert!(state.settle_authenticated(&lease, &live, &reply));
        assert!(begin_waiter.try_recv().unwrap().is_ok());
        let _abort_waiter = state
            .register(
                "lost-abort".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_abort".into(),
                Some("lost-task".into()),
                Some(lost.handle_id.clone()),
            )
            .unwrap();
        uploads
            .require_upload_binding(&lost.handle_id, &lease, "session-1", "lost-task")
            .unwrap();
        assert!(uploads.abort(&lost.handle_id));
        // Test seam: B succeeded, but the authenticated reply is intentionally never delivered.
        state.detach_uncertain("lost-abort");
        assert!(
            state
                .validate_completed_uploads(
                    "local-1",
                    "session-1",
                    &live,
                    "lost-task",
                    std::slice::from_ref(&lost.handle_id)
                )
                .is_err()
        );
        assert_eq!(
            uploads
                .require_upload_binding(&lost.handle_id, &lease, "session-1", "lost-task")
                .unwrap_err(),
            "transfer_unknown"
        );
        let mut retry = state
            .register(
                "lost-abort-retry".into(),
                "local-1".into(),
                "session-1".into(),
                live.clone(),
                "attachment_abort".into(),
                Some("lost-task".into()),
                Some(lost.handle_id.clone()),
            )
            .unwrap();
        let reply = opened(
            json!({"request_id":"lost-abort-retry","session_id":"session-1","operation":"attachment_abort","ok":false,"error_code":"transfer_unknown"}),
        );
        assert!(state.settle_authenticated(&lease, &live, &reply));
        assert_eq!(retry.try_recv().unwrap().unwrap()["already_absent"], true);
    }
    #[test]
    fn twenty_mib_upload_reaches_staged_parser_and_releases_read_owner() {
        use crate::features::remote_control::controller::transfer::{
            TransferContext, TransferDirection, TransferMetadata, TransferRegistry,
            staging::SessionIsolationStaging,
            upload::{AttachmentUploads, UploadProgress},
        };
        use sha2::{Digest, Sha256};

        let root = tempfile::tempdir().unwrap();
        let resolved_root = root.path().to_owned();
        let staging = Arc::new(SessionIsolationStaging::new(Arc::new(move |_| {
            Ok(resolved_root.clone())
        })));
        let uploads = AttachmentUploads::new(Arc::new(TransferRegistry::default()), staging);
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
        const TWENTY_MIB: usize = 20 * 1024 * 1024;
        assert_eq!(
            crate::features::files::file_ingest::MAX_FILE_BYTES,
            TWENTY_MIB as u64
        );
        let mut bytes = vec![b'a'; TWENTY_MIB];
        bytes[..12].copy_from_slice(b"start-marker");
        let end = bytes.len();
        bytes[end - 10..].copy_from_slice(b"end-marker");
        let metadata = TransferMetadata {
            file_name: "report.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: bytes.len() as u64,
            sha256: Sha256::digest(&bytes)
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect(),
        };
        let begin = uploads.begin(context, metadata, 1).unwrap();
        let mut last = None;
        for (index, chunk) in bytes.chunks(begin.chunk_bytes).enumerate() {
            last = Some(
                uploads
                    .accept_chunk(&begin.handle_id, index as u64, chunk, 2)
                    .unwrap(),
            );
        }
        assert!(
            matches!(last, Some(UploadProgress::Completed { bytes: n, .. }) if n == bytes.len() as u64)
        );
        let ids = std::slice::from_ref(&begin.handle_id);
        let sequence = 7;
        uploads
            .claim_for_admission(ids, &lease, "session-0001", "task-0001", sequence)
            .unwrap();
        let descriptors = uploads
            .consume_for_admission(ids, &lease, "session-0001", "task-0001", sequence)
            .unwrap();
        let path = descriptors[0].path.clone();
        assert_eq!(uploads.cleanup_for_admission(sequence + 1), 0);
        assert_eq!(uploads.cleanup_for_admission(sequence), 1);
        assert!(path.exists(), "the consumed read owner must defer cleanup");
        let parsed = descriptors
            .iter()
            .map(|attachment| file_ingest::ingest_staged_attachment(root.path(), attachment))
            .collect::<Result<Vec<_>, _>>()
            .unwrap();
        assert_eq!(parsed.len(), 1);
        assert_eq!(parsed[0].kind, "text");
        assert_eq!(parsed[0].byte_size, bytes.len() as u64);
        assert!(parsed[0].warning.is_none());
        let text = parsed[0].markdown.as_deref().unwrap();
        assert_eq!(text.as_bytes(), bytes.as_slice());
        drop(parsed);
        drop(descriptors);
        uploads.expire(u64::MAX);
        assert!(
            !path.exists(),
            "cleanup completes when the last owner is released"
        );
    }

    #[tokio::test]
    async fn cancelled_waiter_keeps_staged_file_until_blocking_reader_finishes() {
        use crate::features::remote_control::controller::transfer::{
            TransferContext, TransferDirection, TransferMetadata, TransferRegistry,
            staging::SessionIsolationStaging, upload::AttachmentUploads,
        };
        use sha2::{Digest, Sha256};
        let root = tempfile::tempdir().unwrap();
        let root_path = root.path().to_owned();
        let staging_root = root_path.clone();
        let staging = Arc::new(SessionIsolationStaging::new(Arc::new(move |_| {
            Ok(staging_root.clone())
        })));
        let uploads = AttachmentUploads::new(Arc::new(TransferRegistry::default()), staging);
        let lease = LeaseContext {
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-1".into(),
            remote_control_generation: "generation-1".into(),
        };
        let context =
            TransferContext::new(&lease, "session-1", "task-1", TransferDirection::Upload).unwrap();
        let metadata = TransferMetadata {
            file_name: "test.txt".into(),
            mime_type: "text/plain".into(),
            total_bytes: 3,
            sha256: Sha256::digest(b"abc")
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect(),
        };
        let begin = uploads.begin(context, metadata, 1).unwrap();
        uploads
            .accept_chunk(&begin.handle_id, 0, b"abc", 2)
            .unwrap();
        let ids = std::slice::from_ref(&begin.handle_id);
        uploads
            .claim_for_admission(ids, &lease, "session-1", "task-1", 7)
            .unwrap();
        let descriptors = uploads
            .consume_for_admission(ids, &lease, "session-1", "task-1", 7)
            .unwrap();
        let path = descriptors[0].path.clone();
        let (ready_tx, ready_rx) = std::sync::mpsc::channel();
        let (resume_tx, resume_rx) = std::sync::mpsc::channel();
        let (done_tx, done_rx) = std::sync::mpsc::channel();
        let worker = tokio::task::spawn_blocking(move || {
            ready_tx.send(()).unwrap();
            resume_rx.recv().unwrap();
            let parsed =
                file_ingest::ingest_staged_attachment(&root_path, &descriptors[0]).unwrap();
            assert_eq!(parsed.markdown.as_deref(), Some("abc"));
            drop(descriptors);
            done_tx.send(()).unwrap();
        });
        ready_rx
            .recv_timeout(std::time::Duration::from_secs(10))
            .unwrap();
        assert_eq!(uploads.cleanup_for_admission(7), 1);
        worker.abort();
        drop(worker);
        assert!(path.exists());
        resume_tx.send(()).unwrap();
        done_rx
            .recv_timeout(std::time::Duration::from_secs(10))
            .unwrap();
        uploads.expire(u64::MAX);
        assert!(!path.exists());
    }
}
