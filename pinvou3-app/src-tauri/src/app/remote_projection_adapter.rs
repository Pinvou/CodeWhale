//! DEV-30 application composition for typed remote-control projection.
//! The remote-control feature owns only its ingress port and event envelope;
//! Sessions owns durable projection; this adapter performs the conversion.

use std::sync::Arc;

use crate::features::remote_control::controller::event_envelope::*;
use crate::features::remote_control::controller::ports::{
    RemotePortError, RemotePortFuture, RemoteSessionProjectionPort,
};
use crate::features::sessions::application::dispatch_remote::{
    RemoteTerminalCandidate, RemoteTerminalVerificationPort, VerifiedRemoteTerminal,
};
use crate::features::sessions::application::ports::UseCaseError;
use crate::features::sessions::application::project_remote::{
    ProjectionBinding, ProjectionCandidate, ProjectionPayload, ProjectionReport,
    ProjectionVerificationPort, RemoteProjectionService, VerifiedRemoteProjection,
};
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
use crate::features::sessions::remote_proxy::schema::{
    AuthenticatedCursor, AuthenticatedProjection, ProjectionAttachment, ProjectionEntry,
    ProjectionKind, SnapshotCandidate,
};

fn live(lease: &EventLease) -> LiveRemoteContext {
    LiveRemoteContext {
        peer_id: lease.peer_id().to_owned(),
        resume_boundary_id: lease.resume_boundary_id().to_owned(),
        connection_id: lease.connection_id().to_owned(),
        controller_lease_id: lease.controller_lease_id().to_owned(),
        lease_epoch: lease.lease_epoch().to_owned(),
        relay_epoch: lease.relay_epoch().to_owned(),
        generation: lease.generation().to_owned(),
        control_connected: true,
        encrypted_channel_connected: true,
    }
}

fn projection_kind(kind: DisplayKind) -> ProjectionKind {
    match kind {
        DisplayKind::UserMessage => ProjectionKind::UserMessage,
        DisplayKind::AssistantMessage => ProjectionKind::AssistantMessage,
        DisplayKind::Reasoning => ProjectionKind::Reasoning,
        DisplayKind::Tool => ProjectionKind::Tool,
        DisplayKind::Approval => ProjectionKind::Approval,
        DisplayKind::Status => ProjectionKind::Status,
        DisplayKind::Error => ProjectionKind::Error,
        DisplayKind::Attachment => ProjectionKind::Attachment,
        DisplayKind::Artifact => ProjectionKind::Artifact,
        DisplayKind::SnapshotMarker => ProjectionKind::SnapshotMarker,
    }
}

fn projection_entry(display: &JournalDisplay) -> ProjectionEntry {
    ProjectionEntry {
        journal_epoch: display.journal_epoch().to_owned(),
        sequence: display.sequence(),
        event_id: display.event_id().to_owned(),
        kind: projection_kind(display.kind()),
        task_id: Some(display.task_id().to_owned()),
        body: display.body().map(str::to_owned),
        attachments: display
            .attachments()
            .iter()
            .map(|item| ProjectionAttachment {
                id: item.id().to_owned(),
                display_name: item.display_name().to_owned(),
                content_type: item.content_type().to_owned(),
                byte_length: item.byte_length(),
                checksum_sha256: item.checksum_sha256().to_owned(),
            })
            .collect(),
    }
}

fn projection_snapshot(display: &SnapshotDisplay) -> SnapshotCandidate {
    SnapshotCandidate {
        projection: AuthenticatedProjection {
            schema_version: 1,
            entries: display.entries().iter().map(projection_entry).collect(),
        },
        cursor: AuthenticatedCursor {
            journal_epoch: display.journal_epoch().to_owned(),
            sequence: display.sequence(),
        },
    }
}

fn projection_binding(binding: &ProxySessionBinding) -> ProjectionBinding {
    ProjectionBinding {
        local_session_id: binding.local_session_id().to_owned(),
        remote_session_ref: binding.remote_session_ref().to_owned(),
        controlled_session_id: binding.controlled_session_id().to_owned(),
        live: live(binding.lease()),
    }
}

struct TerminalProof<'a> {
    event: &'a AuthenticatedProxyEvent,
}

impl RemoteTerminalVerificationPort for TerminalProof<'_> {
    fn verify_terminal(&self, candidate: &RemoteTerminalCandidate) -> Result<(), UseCaseError> {
        let source = self.event.candidate();
        let ProxyEventKind::Terminal(terminal) = source.kind() else {
            return Err(UseCaseError::new("remote_terminal_proof_mismatch", false));
        };
        let binding = source.binding();
        if candidate.local_session_id != binding.local_session_id()
            || candidate.remote_session_ref != binding.remote_session_ref()
            || candidate.controlled_session_id != binding.controlled_session_id()
            || candidate.live != live(binding.lease())
            || candidate.task_id != terminal.journal().task_id()
            || candidate.client_request_id != terminal.client_request_id()
            || candidate.request_fingerprint != terminal.request_fingerprint()
            || candidate.authenticated_event_id != terminal.journal().event_id()
        {
            return Err(UseCaseError::new("remote_terminal_proof_mismatch", false));
        }
        Ok(())
    }
}

fn projection_candidate(
    event: &AuthenticatedProxyEvent,
) -> Result<ProjectionCandidate, UseCaseError> {
    let source = event.candidate();
    let binding = projection_binding(source.binding());
    let payload = match source.kind() {
        ProxyEventKind::Journal(display) => ProjectionPayload::Journal(projection_entry(display)),
        ProxyEventKind::Snapshot(display) => {
            ProjectionPayload::Snapshot(projection_snapshot(display))
        }
        ProxyEventKind::Terminal(terminal) => {
            let proof = VerifiedRemoteTerminal::verify(
                RemoteTerminalCandidate {
                    local_session_id: binding.local_session_id.clone(),
                    remote_session_ref: binding.remote_session_ref.clone(),
                    controlled_session_id: binding.controlled_session_id.clone(),
                    task_id: terminal.journal().task_id().to_owned(),
                    client_request_id: terminal.client_request_id().to_owned(),
                    request_fingerprint: terminal.request_fingerprint().to_owned(),
                    authenticated_event_id: terminal.journal().event_id().to_owned(),
                    live: binding.live.clone(),
                },
                &TerminalProof { event },
            )?;
            ProjectionPayload::Terminal {
                entry: projection_entry(terminal.journal()),
                status: terminal.status().as_str().to_owned(),
                proof: Box::new(proof),
            }
        }
    };
    Ok(ProjectionCandidate { binding, payload })
}

struct EnvelopeProof<'a> {
    event: &'a AuthenticatedProxyEvent,
}

impl ProjectionVerificationPort for EnvelopeProof<'_> {
    fn verify(&self, candidate: &ProjectionCandidate) -> Result<(), UseCaseError> {
        let source = self.event.candidate();
        let matched = candidate.binding == projection_binding(source.binding())
            && match (&candidate.payload, source.kind()) {
                (ProjectionPayload::Journal(left), ProxyEventKind::Journal(right)) => {
                    left == &projection_entry(right)
                }
                (ProjectionPayload::Snapshot(left), ProxyEventKind::Snapshot(right)) => {
                    left.cursor == projection_snapshot(right).cursor
                        && left.projection == projection_snapshot(right).projection
                }
                (
                    ProjectionPayload::Terminal { entry, status, .. },
                    ProxyEventKind::Terminal(right),
                ) => {
                    entry == &projection_entry(right.journal()) && status == right.status().as_str()
                }
                _ => false,
            };
        if matched {
            Ok(())
        } else {
            Err(UseCaseError::new("remote_projection_proof_mismatch", false))
        }
    }
}

/// Composition-root-injectable adapter: encrypted ingress supplies the
/// authenticated event, then the session use case owns all durable writes.
pub(crate) struct RemoteProjectionAdapter {
    projector: Arc<RemoteProjectionService>,
}

impl RemoteProjectionAdapter {
    pub(crate) fn new(projector: Arc<RemoteProjectionService>) -> Self {
        Self { projector }
    }

    /// Synchronous ingress for the existing lane callback. It executes the
    /// same typed proof and session use case as the async port, so the caller
    /// can acknowledge only after the durable writer returns.
    pub(crate) fn project_authenticated_now(
        &self,
        event: AuthenticatedProxyEvent,
    ) -> Result<ProjectionReport, RemotePortError> {
        let candidate = projection_candidate(&event)
            .map_err(|error| RemotePortError::new(error.code(), error.is_retryable()))?;
        let verified =
            VerifiedRemoteProjection::verify(candidate, &EnvelopeProof { event: &event })
                .map_err(|error| RemotePortError::new(error.code(), error.is_retryable()))?;
        self.projector
            .project_now(verified)
            .map_err(|error| RemotePortError::new(error.code(), error.is_retryable()))
    }
}

impl RemoteSessionProjectionPort for RemoteProjectionAdapter {
    type Event = AuthenticatedProxyEvent;
    type Projection = ProjectionReport;

    fn project_authenticated(&self, event: Self::Event) -> RemotePortFuture<'_, Self::Projection> {
        Box::pin(async move { self.project_authenticated_now(event) })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::sessions::application::ports::{ApplicationEvent, ApplicationEventPort};
    use std::sync::atomic::{AtomicBool, Ordering};

    use crate::features::sessions::SessionStore;
    use crate::features::sessions::remote_proxy::binding::AuthenticatedMapping;
    use crate::features::sessions::remote_proxy::schema::{
        PendingDispatchState, PendingRemoteRequest,
    };
    use parking_lot::Mutex;

    struct Reject;
    impl ProxyEventVerificationPort for Reject {
        fn verify(&self, _: &ProxyEventCandidate) -> Result<(), RemotePortError> {
            Err(RemotePortError::new("authentication_failed", false))
        }
    }

    struct Accept;
    impl ProxyEventVerificationPort for Accept {
        fn verify(&self, _: &ProxyEventCandidate) -> Result<(), RemotePortError> {
            Ok(())
        }
    }

    fn lease() -> EventLease {
        EventLease::new(
            "peer-1".into(),
            "resume-1".into(),
            "connection-1".into(),
            "lease-1".into(),
            "1".into(),
            "relay-1".into(),
            "generation-1".into(),
        )
        .unwrap()
    }

    #[derive(Default)]
    struct RecordingEvents {
        events: Mutex<Vec<ApplicationEvent>>,
        fail: AtomicBool,
    }

    impl ApplicationEventPort for RecordingEvents {
        fn publish(&self, event: ApplicationEvent) -> Result<(), UseCaseError> {
            if self.fail.load(Ordering::SeqCst) {
                return Err(UseCaseError::new("host_event_failed", false));
            }
            self.events.lock().push(event);
            Ok(())
        }
    }

    fn open(store: &SessionStore) -> ProxySessionBinding {
        let live = live(&lease());
        let opened = store.test_commit_remote_proxy_open(None, &live).unwrap();
        store
            .confirm_remote_mapping(
                &opened.local_session_id,
                &live,
                &AuthenticatedMapping {
                    remote_session_ref: opened.remote_session_ref.clone(),
                    controlled_session_id: "b-session-1".into(),
                },
            )
            .unwrap();
        ProxySessionBinding::new(
            lease(),
            opened.local_session_id,
            opened.remote_session_ref,
            "b-session-1".into(),
        )
        .unwrap()
    }

    fn journal(sequence: u64, kind: DisplayKind) -> JournalDisplay {
        JournalDisplay::new(
            "journal-1".into(),
            sequence,
            format!("event-{sequence}"),
            kind,
            "task-1".into(),
            Some("display text".into()),
            Vec::new(),
        )
        .unwrap()
    }

    fn authenticated(candidate: ProxyEventCandidate) -> AuthenticatedProxyEvent {
        AuthenticatedProxyEvent::verify(candidate, &Accept).unwrap()
    }

    #[test]
    fn connection_and_b_events_have_distinct_required_bindings() {
        let control = ConnectionEvent::new(
            lease(),
            ConnectionRole::Controller,
            ConnectionEventKind::Disconnected,
        );
        assert_eq!(control.lease().generation(), "generation-1");
        let b = ControlledSessionBinding::new(lease(), "b-1".into(), "ref-1".into()).unwrap();
        assert!(matches!(
            ControlledSessionEvent::capability(b.clone(), 2).view(),
            ControlledSessionView::Capability { revision: 2, .. }
        ));
        assert!(ControlledSessionEvent::task(b.clone(), String::new()).is_err());
        assert!(
            ControlledSessionEvent::journal(
                b,
                "task-1".into(),
                "epoch-1".into(),
                0,
                "event-1".into()
            )
            .is_err()
        );
        assert!(
            ProxySessionBinding::new(lease(), "b-1".into(), "ref-1".into(), "b-1".into()).is_err()
        );
    }

    #[test]
    fn proxy_variants_validate_fields_and_require_authentication() {
        let binding =
            ProxySessionBinding::new(lease(), "remote-local".into(), "ref-1".into(), "b-1".into())
                .unwrap();
        let journal = JournalDisplay::new(
            "epoch-1".into(),
            1,
            "event-1".into(),
            DisplayKind::Status,
            "task-1".into(),
            None,
            Vec::new(),
        )
        .unwrap();
        assert!(
            TerminalDisplay::new(
                journal.clone(),
                String::new(),
                "a".repeat(64),
                TerminalStatus::Succeeded
            )
            .is_err()
        );
        assert!(SnapshotDisplay::new("epoch-1".into(), 2, vec![journal.clone()]).is_err());
        let candidate = ProxyEventCandidate::journal(binding, journal).unwrap();
        assert_eq!(
            AuthenticatedProxyEvent::verify(candidate.clone(), &Reject)
                .err()
                .unwrap()
                .code(),
            "authentication_failed"
        );
        assert!(matches!(
            AuthenticatedProxyEvent::verify(candidate, &Accept)
                .unwrap()
                .candidate()
                .kind(),
            ProxyEventKind::Journal(_)
        ));
    }

    #[tokio::test]
    async fn adapter_only_notifies_after_commit_and_ignores_duplicate_or_stale_events() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let binding = open(&store);
        let bus = Arc::new(RecordingEvents::default());
        let projector = Arc::new(RemoteProjectionService::new(store.clone(), bus.clone()));
        let adapter = RemoteProjectionAdapter::new(projector);
        let first = ProxyEventCandidate::journal(
            binding.clone(),
            journal(1, DisplayKind::AssistantMessage),
        )
        .unwrap();
        assert!(AuthenticatedProxyEvent::verify(first.clone(), &Reject).is_err());
        assert!(
            store
                .remote_proxy_snapshot(binding.local_session_id())
                .unwrap()
                .projection
                .entries
                .is_empty()
        );
        bus.fail.store(true, Ordering::SeqCst);
        assert_eq!(
            adapter
                .project_authenticated(authenticated(first.clone()))
                .await
                .unwrap(),
            ProjectionReport {
                committed: true,
                notified: false
            }
        );
        assert_eq!(
            store
                .remote_proxy_snapshot(binding.local_session_id())
                .unwrap()
                .projection
                .entries
                .len(),
            1
        );
        bus.fail.store(false, Ordering::SeqCst);
        assert_eq!(
            adapter
                .project_authenticated(authenticated(first))
                .await
                .unwrap(),
            ProjectionReport {
                committed: false,
                notified: false
            }
        );
        assert!(bus.events.lock().is_empty());
        let second = ProxyEventCandidate::journal(
            binding.clone(),
            journal(2, DisplayKind::AssistantMessage),
        )
        .unwrap();
        assert_eq!(
            adapter
                .project_authenticated(authenticated(second))
                .await
                .unwrap(),
            ProjectionReport {
                committed: true,
                notified: true
            }
        );
        assert_eq!(bus.events.lock().len(), 1);
        let wrong_b = ProxySessionBinding::new(
            binding.lease().clone(),
            binding.local_session_id().into(),
            binding.remote_session_ref().into(),
            "other-b".into(),
        )
        .unwrap();
        assert!(
            adapter
                .project_authenticated(authenticated(
                    ProxyEventCandidate::journal(
                        wrong_b,
                        journal(3, DisplayKind::AssistantMessage)
                    )
                    .unwrap()
                ))
                .await
                .is_err()
        );
        store.test_expire_remote_binding(binding.local_session_id());
        assert!(
            adapter
                .project_authenticated(authenticated(
                    ProxyEventCandidate::journal(
                        binding.clone(),
                        journal(3, DisplayKind::AssistantMessage)
                    )
                    .unwrap()
                ))
                .await
                .is_err()
        );
        assert_eq!(bus.events.lock().len(), 1);
    }

    #[tokio::test]
    async fn terminal_status_is_canonical_and_pending_retires_with_projection() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let binding = open(&store);
        let live = live(binding.lease());
        let fingerprint = "a".repeat(64);
        store
            .insert_remote_pending(
                binding.local_session_id(),
                binding.remote_session_ref(),
                &live,
                PendingRemoteRequest {
                    client_request_id: "request-1".into(),
                    task_id: "task-1".into(),
                    request_fingerprint: fingerprint.clone(),
                    dispatch_state: PendingDispatchState::PreparedNeverAttempted,
                    reservation_fingerprint: None,
                },
            )
            .unwrap();
        store
            .mark_remote_dispatch_attempt(
                binding.local_session_id(),
                binding.remote_session_ref(),
                &live,
                "request-1",
                "task-1",
                &fingerprint,
            )
            .unwrap();
        let terminal = TerminalDisplay::new(
            journal(1, DisplayKind::Status),
            "request-1".into(),
            fingerprint,
            TerminalStatus::Succeeded,
        )
        .unwrap();
        assert_eq!(terminal.journal().body(), Some("succeeded"));
        let candidate = ProxyEventCandidate::terminal(binding.clone(), terminal).unwrap();
        let bus = Arc::new(RecordingEvents::default());
        let adapter = RemoteProjectionAdapter::new(Arc::new(RemoteProjectionService::new(
            store.clone(),
            bus.clone(),
        )));
        assert_eq!(
            adapter
                .project_authenticated(authenticated(candidate.clone()))
                .await
                .unwrap(),
            ProjectionReport {
                committed: true,
                notified: true
            }
        );
        assert_eq!(
            adapter
                .project_authenticated(authenticated(candidate))
                .await
                .unwrap(),
            ProjectionReport {
                committed: false,
                notified: false
            }
        );
        let record = store
            .remote_proxy_snapshot(binding.local_session_id())
            .unwrap();
        assert!(record.pending.is_empty());
        assert_eq!(record.retired_requests.len(), 1);
        assert_eq!(
            record.projection.entries[0].body.as_deref(),
            Some("succeeded")
        );
        assert_eq!(bus.events.lock().len(), 1);
        assert_eq!(
            bus.events.lock()[0].payload()["fields"]["status"],
            "succeeded"
        );
    }

    #[tokio::test]
    async fn snapshot_binds_exact_mapping_and_cursor() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let binding = open(&store);
        let bus = Arc::new(RecordingEvents::default());
        let adapter = RemoteProjectionAdapter::new(Arc::new(RemoteProjectionService::new(
            store.clone(),
            bus.clone(),
        )));
        let display = SnapshotDisplay::new(
            "journal-1".into(),
            1,
            vec![journal(1, DisplayKind::AssistantMessage)],
        )
        .unwrap();
        let snap = ProxyEventCandidate::snapshot(binding.clone(), display.clone()).unwrap();
        assert_eq!(
            adapter
                .project_authenticated(authenticated(snap.clone()))
                .await
                .unwrap(),
            ProjectionReport {
                committed: true,
                notified: true
            }
        );
        assert_eq!(
            adapter
                .project_authenticated(authenticated(snap.clone()))
                .await
                .unwrap(),
            ProjectionReport {
                committed: false,
                notified: false
            }
        );
        assert_eq!(bus.events.lock().len(), 1);
        let wrong_b = ProxySessionBinding::new(
            binding.lease().clone(),
            binding.local_session_id().into(),
            binding.remote_session_ref().into(),
            "other-b".into(),
        )
        .unwrap();
        assert!(
            adapter
                .project_authenticated(authenticated(
                    ProxyEventCandidate::snapshot(wrong_b, display).unwrap()
                ))
                .await
                .is_err()
        );
        store.test_expire_remote_binding(binding.local_session_id());
        assert!(
            adapter
                .project_authenticated(authenticated(snap))
                .await
                .is_err()
        );
        assert_eq!(bus.events.lock().len(), 1);
    }

    #[test]
    fn invalid_ids_and_attachments_cannot_be_constructed() {
        assert!(
            EventLease::new(
                "".into(),
                "resume-1".into(),
                "connection-1".into(),
                "lease-1".into(),
                "1".into(),
                "relay-1".into(),
                "generation-1".into(),
            )
            .is_err()
        );
        assert!(
            ControlledSessionBinding::new(lease(), "b with spaces".into(), "ref-1".into()).is_err()
        );
        assert!(
            ProxySessionBinding::new(lease(), "remote-good".into(), "".into(), "b-1".into())
                .is_err()
        );
        assert!(
            AttachmentIndex::new(
                "".into(),
                "name".into(),
                "text/plain".into(),
                1,
                "a".repeat(64)
            )
            .is_err()
        );
        assert!(
            AttachmentIndex::new(
                "attachment-1".into(),
                "name".into(),
                "text/plain".into(),
                1,
                "bad".into()
            )
            .is_err()
        );
        let good = AttachmentIndex::new(
            "attachment-1".into(),
            "name".into(),
            "text/plain".into(),
            1,
            "a".repeat(64),
        )
        .unwrap();
        assert_eq!(good.id(), "attachment-1");
        assert!(
            JournalDisplay::new(
                "epoch-1".into(),
                1,
                "event-1".into(),
                DisplayKind::Attachment,
                "task-1".into(),
                None,
                vec![good]
            )
            .is_ok()
        );
    }
}
