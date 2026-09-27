//! Tauri-independent A-side projection use case. No remote-control feature
//! dependency: the composition adapter converts authenticated wire envelopes
//! into this module's verified, session-owned DTOs.

use serde_json::json;
use std::sync::Arc;

use super::dispatch_remote::VerifiedRemoteTerminal;
use super::ports::{ApplicationEvent, ApplicationEventPort, UseCaseError, UseCaseFuture};
use crate::features::sessions::SessionStore;
use crate::features::sessions::remote_proxy::binding::LiveRemoteContext;
use crate::features::sessions::remote_proxy::projection::ProjectionWrite;
use crate::features::sessions::remote_proxy::schema::{
    ProjectionEntry, SnapshotCandidate, SnapshotVerificationPort,
};

pub(crate) trait ProjectRemoteUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn project(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}

fn error(code: &'static str) -> UseCaseError {
    UseCaseError::new(code, false)
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ProjectionBinding {
    pub(crate) local_session_id: String,
    pub(crate) remote_session_ref: String,
    pub(crate) controlled_session_id: String,
    pub(crate) live: LiveRemoteContext,
}

pub(crate) enum ProjectionPayload {
    Journal(ProjectionEntry),
    Snapshot(SnapshotCandidate),
    Terminal {
        entry: ProjectionEntry,
        status: String,
        proof: VerifiedRemoteTerminal,
    },
}

pub(crate) struct ProjectionCandidate {
    pub(crate) binding: ProjectionBinding,
    pub(crate) payload: ProjectionPayload,
}

/// Verifies the full candidate against an already authenticated ingress
/// token; this is not a caller-controlled `authenticated` boolean.
pub(crate) trait ProjectionVerificationPort {
    fn verify(&self, candidate: &ProjectionCandidate) -> Result<(), UseCaseError>;
}

pub(crate) struct VerifiedRemoteProjection {
    candidate: ProjectionCandidate,
}

impl VerifiedRemoteProjection {
    pub(crate) fn verify(
        candidate: ProjectionCandidate,
        verifier: &dyn ProjectionVerificationPort,
    ) -> Result<Self, UseCaseError> {
        verifier.verify(&candidate)?;
        Ok(Self { candidate })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct ProjectionReport {
    pub(crate) committed: bool,
    pub(crate) notified: bool,
}

/// Only UI notification order is owned here. The authoritative content,
/// cursor and terminal retirement are committed by SessionStore first.
pub(crate) struct RemoteProjectionService {
    store: SessionStore,
    events: Arc<dyn ApplicationEventPort>,
}

impl RemoteProjectionService {
    pub(crate) fn new(store: SessionStore, events: Arc<dyn ApplicationEventPort>) -> Self {
        Self { store, events }
    }

    fn publish(
        &self,
        binding: &ProjectionBinding,
        kind: &'static str,
        fields: serde_json::Value,
    ) -> bool {
        self.events
            .publish(ApplicationEvent::new(
                "desktop_remote_proxy",
                json!({
                    "kind": kind,
                    "localSessionId": binding.local_session_id,
                    "remoteSessionRef": binding.remote_session_ref,
                    "controlledSessionId": binding.controlled_session_id,
                    "peerId": binding.live.peer_id,
                    "resumeBoundaryId": binding.live.resume_boundary_id,
                    "connectionId": binding.live.connection_id,
                    "controllerLeaseId": binding.live.controller_lease_id,
                    "leaseEpoch": binding.live.lease_epoch,
                    "relayEpoch": binding.live.relay_epoch,
                    "generation": binding.live.generation,
                    "fields": fields,
                }),
            ))
            .is_ok()
    }

    pub(crate) fn project_now(
        &self,
        verified: VerifiedRemoteProjection,
    ) -> Result<ProjectionReport, UseCaseError> {
        let ProjectionCandidate { binding, payload } = verified.candidate;
        match payload {
            ProjectionPayload::Journal(entry) => {
                let notice = json!({
                    "journalEpoch": entry.journal_epoch,
                    "sequence": entry.sequence,
                    "eventId": entry.event_id,
                });
                let write = self
                    .store
                    .project_authenticated_remote_entry(
                        &binding.local_session_id,
                        &binding.remote_session_ref,
                        &binding.live,
                        &binding.controlled_session_id,
                        entry,
                    )
                    .map_err(|_| error("remote_projection_rejected"))?;
                Ok(self.report_write(&binding, write, "journal", notice))
            }
            ProjectionPayload::Snapshot(candidate) => {
                let notice = json!({
                    "journalEpoch": candidate.cursor.journal_epoch,
                    "sequence": candidate.cursor.sequence,
                });
                let trusted = TrustedSnapshot {
                    expected: &candidate,
                };
                let write = self
                    .store
                    .project_authenticated_remote_snapshot(
                        &binding.local_session_id,
                        &binding.remote_session_ref,
                        &binding.live,
                        &binding.controlled_session_id,
                        candidate.clone(),
                        &trusted,
                    )
                    .map_err(|_| error("remote_projection_rejected"))?;
                Ok(self.report_write(&binding, write, "snapshot", notice))
            }
            ProjectionPayload::Terminal {
                entry,
                status,
                proof,
            } => {
                let terminal = proof.into_candidate();
                if terminal.local_session_id != binding.local_session_id
                    || terminal.remote_session_ref != binding.remote_session_ref
                    || terminal.controlled_session_id != binding.controlled_session_id
                    || terminal.live != binding.live
                    || entry.task_id.as_deref() != Some(terminal.task_id.as_str())
                    || entry.event_id != terminal.authenticated_event_id
                    || !matches!(
                        status.as_str(),
                        "succeeded" | "failed" | "cancelled" | "interrupted"
                    )
                    || entry.body.as_deref() != Some(status.as_str())
                {
                    return Err(error("remote_terminal_proof_mismatch"));
                }
                let notice = json!({
                    "journalEpoch": entry.journal_epoch,
                    "sequence": entry.sequence,
                    "eventId": entry.event_id,
                    "status": status,
                });
                let write = self
                    .store
                    .project_authenticated_remote_terminal(
                        &terminal.local_session_id,
                        &terminal.remote_session_ref,
                        &terminal.live,
                        &terminal.controlled_session_id,
                        &terminal.client_request_id,
                        &terminal.task_id,
                        &terminal.request_fingerprint,
                        entry,
                    )
                    .map_err(|_| error("remote_projection_rejected"))?;
                Ok(self.report_write(&binding, write, "terminal", notice))
            }
        }
    }

    fn report_write(
        &self,
        binding: &ProjectionBinding,
        write: ProjectionWrite,
        kind: &'static str,
        fields: serde_json::Value,
    ) -> ProjectionReport {
        if write == ProjectionWrite::Unchanged {
            return ProjectionReport {
                committed: false,
                notified: false,
            };
        }
        ProjectionReport {
            committed: true,
            notified: self.publish(binding, kind, fields),
        }
    }
}

struct TrustedSnapshot<'a> {
    expected: &'a SnapshotCandidate,
}

impl SnapshotVerificationPort for TrustedSnapshot<'_> {
    fn verify_authenticated_snapshot(
        &self,
        _: &str,
        _: &str,
        _: &LiveRemoteContext,
        candidate: &SnapshotCandidate,
    ) -> anyhow::Result<()> {
        if candidate.cursor != self.expected.cursor
            || candidate.projection != self.expected.projection
        {
            anyhow::bail!("snapshot candidate changed after authentication");
        }
        Ok(())
    }
}

impl ProjectRemoteUseCase for RemoteProjectionService {
    type Request = VerifiedRemoteProjection;
    type Response = ProjectionReport;

    fn project(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response> {
        Box::pin(async move { self.project_now(request) })
    }
}
