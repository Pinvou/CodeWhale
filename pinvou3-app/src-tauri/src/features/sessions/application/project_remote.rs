//! Tauri-independent A-side projection use case. No remote-control feature
//! dependency: the composition adapter converts authenticated wire envelopes
//! into this module's verified, session-owned DTOs.

use std::{collections::HashMap, sync::Arc};

use parking_lot::Mutex;
use serde_json::json;

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
    Capability {
        revision: u64,
    },
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

struct CapabilityGate {
    live: LiveRemoteContext,
    committed_revision: Option<u64>,
    in_flight: Option<u64>,
}

/// Only UI notification order is owned here. The authoritative content,
/// cursor and terminal retirement are committed by SessionStore first.
pub(crate) struct RemoteProjectionService {
    store: SessionStore,
    events: Arc<dyn ApplicationEventPort>,
    /// B capability revision has no durable v3 slot. This bounded transient
    /// notification cursor resets with the UI after process restart.
    capability_revisions: Mutex<HashMap<String, CapabilityGate>>,
}

impl RemoteProjectionService {
    pub(crate) fn new(store: SessionStore, events: Arc<dyn ApplicationEventPort>) -> Self {
        Self {
            store,
            events,
            capability_revisions: Mutex::new(HashMap::new()),
        }
    }

    fn require_current(&self, binding: &ProjectionBinding) -> Result<(), UseCaseError> {
        self.store
            .require_live_remote_projection_mapping(
                &binding.local_session_id,
                &binding.remote_session_ref,
                &binding.live,
                &binding.controlled_session_id,
            )
            .map_err(|_| error("remote_projection_binding_mismatch"))
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

    fn project_capability(
        &self,
        binding: &ProjectionBinding,
        revision: u64,
    ) -> Result<ProjectionReport, UseCaseError> {
        self.require_current(binding)?;
        {
            let mut gates = self.capability_revisions.lock();
            if !gates.contains_key(&binding.local_session_id) && gates.len() >= 256 {
                return Err(error("remote_capability_capacity"));
            }
            let gate = gates
                .entry(binding.local_session_id.clone())
                .or_insert_with(|| CapabilityGate {
                    live: binding.live.clone(),
                    committed_revision: None,
                    in_flight: None,
                });
            if gate.live != binding.live {
                *gate = CapabilityGate {
                    live: binding.live.clone(),
                    committed_revision: None,
                    in_flight: None,
                };
            }
            if let Some(previous) = gate.committed_revision {
                if revision < previous {
                    return Err(error("remote_capability_stale"));
                }
                if revision == previous {
                    return Ok(ProjectionReport {
                        committed: false,
                        notified: false,
                    });
                }
            }
            if gate.in_flight.is_some() {
                return Err(UseCaseError::new("remote_capability_busy", true));
            }
            gate.in_flight = Some(revision);
        }
        // The external host event port is never called while holding the
        // capability mutex. A reentrant publish therefore cannot deadlock.
        let still_current = self.require_current(binding).is_ok();
        let notified = still_current
            && self.publish(
                binding,
                "capability",
                json!({ "capabilityRevision": revision }),
            );
        let mut gates = self.capability_revisions.lock();
        if let Some(gate) = gates.get_mut(&binding.local_session_id) {
            if gate.live == binding.live && gate.in_flight == Some(revision) {
                gate.in_flight = None;
                if notified {
                    gate.committed_revision = Some(revision);
                }
            }
        }
        if !still_current {
            return Err(error("remote_projection_binding_mismatch"));
        }
        Ok(ProjectionReport {
            committed: false,
            notified,
        })
    }

    fn project_now(
        &self,
        verified: VerifiedRemoteProjection,
    ) -> Result<ProjectionReport, UseCaseError> {
        let ProjectionCandidate { binding, payload } = verified.candidate;
        match payload {
            ProjectionPayload::Capability { revision } => {
                self.project_capability(&binding, revision)
            }
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Weak;
    use std::sync::atomic::{AtomicBool, Ordering};

    use crate::features::sessions::remote_proxy::binding::AuthenticatedMapping;

    #[derive(Default)]
    struct ReentrantEvents {
        service: Mutex<Option<Weak<RemoteProjectionService>>>,
        binding: Mutex<Option<ProjectionBinding>>,
        fail: AtomicBool,
        reentered: Mutex<Vec<&'static str>>,
        published: Mutex<usize>,
    }

    impl ApplicationEventPort for ReentrantEvents {
        fn publish(&self, _: ApplicationEvent) -> Result<(), UseCaseError> {
            let service = self.service.lock().as_ref().and_then(Weak::upgrade);
            let binding = self.binding.lock().clone();
            if let (Some(service), Some(binding)) = (service, binding) {
                let code = service.project_capability(&binding, 2).unwrap_err().code();
                self.reentered.lock().push(code);
            }
            if self.fail.load(Ordering::SeqCst) {
                return Err(error("event_bus_failed"));
            }
            *self.published.lock() += 1;
            Ok(())
        }
    }

    #[test]
    fn capability_publish_is_outside_gate_lock_and_failure_can_retry() {
        let temp = tempfile::tempdir().unwrap();
        let store = SessionStore::boot_at_test_dir(temp.path()).unwrap();
        let live = LiveRemoteContext {
            peer_id: "peer-1".into(),
            resume_boundary_id: "resume-1".into(),
            connection_id: "connection-1".into(),
            controller_lease_id: "lease-1".into(),
            lease_epoch: "epoch-1".into(),
            relay_epoch: "relay-1".into(),
            generation: "generation-1".into(),
            control_connected: true,
            encrypted_channel_connected: true,
        };
        let opened = store.open_remote_proxy(None, &live).unwrap();
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
        let binding = ProjectionBinding {
            local_session_id: opened.local_session_id,
            remote_session_ref: opened.remote_session_ref,
            controlled_session_id: "b-session-1".into(),
            live,
        };
        let events = Arc::new(ReentrantEvents::default());
        let service = Arc::new(RemoteProjectionService::new(store, events.clone()));
        *events.service.lock() = Some(Arc::downgrade(&service));
        *events.binding.lock() = Some(binding.clone());
        events.fail.store(true, Ordering::SeqCst);
        assert_eq!(
            service.project_capability(&binding, 2).unwrap(),
            ProjectionReport {
                committed: false,
                notified: false
            }
        );
        assert_eq!(
            events.reentered.lock().as_slice(),
            &["remote_capability_busy"]
        );
        events.fail.store(false, Ordering::SeqCst);
        assert_eq!(
            service.project_capability(&binding, 2).unwrap(),
            ProjectionReport {
                committed: false,
                notified: true
            }
        );
        assert_eq!(*events.published.lock(), 1);
        assert_eq!(
            service.project_capability(&binding, 2).unwrap(),
            ProjectionReport {
                committed: false,
                notified: false
            }
        );
        assert_eq!(
            service.project_capability(&binding, 1).unwrap_err().code(),
            "remote_capability_stale"
        );
        assert_eq!(events.reentered.lock().len(), 2);
    }
}
