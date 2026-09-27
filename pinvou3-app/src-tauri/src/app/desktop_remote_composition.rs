//! Host-only ports for the desktop remote composition root in `lib.rs`.

use std::sync::Arc;

use tauri::{Emitter as _, Manager as _};

use super::{commands, remote_projection_adapter};
use crate::features::remote_control::controller::event_envelope::{
    AuthenticatedProxyEvent, ConnectionEvent, ControlledSessionBinding,
};
use crate::features::remote_control::controller::ports::{
    ControlledExecutionPort, RemotePortError, RemotePortFuture, RemoteSessionProjectionPort,
};
use crate::features::sessions::application::ports::{
    ApplicationEvent, ApplicationEventPort, UseCaseError,
};
use crate::features::sessions::application::project_remote::ProjectionReport;
use crate::features::sessions::application::submit_controlled::{
    ControlledSubmissionResult, LocalControlledInput, RemoteControlledInput,
};

pub(crate) type DesktopControlledExecutionPort = Arc<
    dyn ControlledExecutionPort<
            Request = RemoteControlledInput,
            Response = ControlledSubmissionResult,
        >,
>;
pub(crate) type DesktopLocalExecutionPort = Arc<dyn LocalControlledExecutionPort>;
pub(crate) type DesktopRemoteProjectionPort = Arc<
    dyn RemoteSessionProjectionPort<Event = AuthenticatedProxyEvent, Projection = ProjectionReport>,
>;

/// B-local input shares the one concrete execution owner with authenticated
/// remote input, without widening the remote execution trait's request type.
pub(crate) trait LocalControlledExecutionPort: Send + Sync {
    fn submit_local(
        &self,
        request: LocalControlledInput,
    ) -> RemotePortFuture<'_, ControlledSubmissionResult>;
}

impl LocalControlledExecutionPort
    for commands::desktop_remote_controller_business::DesktopControlledExecutionAdapter
{
    fn submit_local(
        &self,
        request: LocalControlledInput,
    ) -> RemotePortFuture<'_, ControlledSubmissionResult> {
        Box::pin(
            commands::desktop_remote_controller_business::DesktopControlledExecutionAdapter::submit_local(
                self, request,
            ),
        )
    }
}

/// The sessions use case calls this only after its durable projection write.
pub(crate) struct DesktopRemoteApplicationEvents(pub(crate) tauri::AppHandle);

impl ApplicationEventPort for DesktopRemoteApplicationEvents {
    fn publish(&self, event: ApplicationEvent) -> Result<(), UseCaseError> {
        self.0
            .emit(event.topic(), event.payload())
            .map_err(|_| UseCaseError::new("remote_event_publish_failed", true))?;
        crate::platform::app_events::forward_app_event(
            &self.0,
            event.topic(),
            event.payload().clone(),
        );
        Ok(())
    }
}

/// The current role owners expose only five of EventLease's seven identity
/// fields. Until DEV-31 injects a single authoritative peer/resume verifier,
/// host notifications must fail closed even when those five fields match.
pub(crate) struct DesktopRemoteEventFreshness;

impl remote_projection_adapter::ControllerEventFreshnessPort for DesktopRemoteEventFreshness {
    fn verify_connection(&self, _event: &ConnectionEvent) -> Result<(), UseCaseError> {
        Err(UseCaseError::new(
            "remote_event_binding_unverifiable",
            false,
        ))
    }

    fn verify_controlled(&self, _binding: &ControlledSessionBinding) -> Result<(), UseCaseError> {
        Err(UseCaseError::new(
            "remote_event_binding_unverifiable",
            false,
        ))
    }
}

/// DEV-31's authenticated ingress can consume only this typed seam. No
/// JournalEvent or UI JSON can be promoted into an AuthenticatedProxyEvent.
#[allow(dead_code)]
pub(crate) async fn project_authenticated_desktop_remote_event(
    app: &tauri::AppHandle,
    event: AuthenticatedProxyEvent,
) -> Result<ProjectionReport, RemotePortError> {
    app.state::<DesktopRemoteProjectionPort>()
        .project_authenticated(event)
        .await
}

#[cfg(test)]
mod tests {
    use super::remote_projection_adapter::ControllerEventFreshnessPort;
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    struct RecordingEvents(AtomicUsize);

    impl ApplicationEventPort for RecordingEvents {
        fn publish(&self, _event: ApplicationEvent) -> Result<(), UseCaseError> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }
    }

    #[test]
    fn host_notifications_fail_closed_without_full_authoritative_lease() {
        use crate::features::remote_control::controller::event_envelope::{
            ConnectionEventKind, ConnectionRole, EventLease,
        };
        let event = EventLease::new(
            "peer-1".into(),
            "resume-1".into(),
            "connection-1".into(),
            "lease-1".into(),
            "epoch-1".into(),
            "relay-1".into(),
            "generation-1".into(),
        )
        .unwrap();
        let freshness = DesktopRemoteEventFreshness;
        for role in [ConnectionRole::Controller, ConnectionRole::Controlled] {
            let connection =
                ConnectionEvent::new(event.clone(), role, ConnectionEventKind::Connected);
            assert_eq!(
                freshness.verify_connection(&connection).unwrap_err().code(),
                "remote_event_binding_unverifiable"
            );
        }
        let controlled =
            ControlledSessionBinding::new(event, "b-session-1".into(), "remote-ref-1".into())
                .unwrap();
        assert_eq!(
            freshness.verify_controlled(&controlled).unwrap_err().code(),
            "remote_event_binding_unverifiable"
        );
        let events = Arc::new(RecordingEvents(AtomicUsize::new(0)));
        let host = remote_projection_adapter::ControllerHostEventAdapter::new(
            events.clone(),
            Arc::new(DesktopRemoteEventFreshness),
        );
        assert!(
            host.publish_connection(ConnectionEvent::new(
                controlled.lease().clone(),
                ConnectionRole::Controller,
                ConnectionEventKind::Connected,
            ))
            .is_err()
        );
        assert!(
            host.publish_controlled(
                crate::features::remote_control::controller::event_envelope::ControlledSessionEvent::capability(
                    controlled,
                    1,
                ),
            )
            .is_err()
        );
        assert_eq!(events.0.load(Ordering::SeqCst), 0);
    }
}
