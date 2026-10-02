//! Host-only ports for the desktop remote composition root in `lib.rs`.

use std::sync::Arc;

use tauri::Emitter as _;

use super::commands;
use crate::features::remote_control::controller::event_envelope::AuthenticatedProxyEvent;
use crate::features::remote_control::controller::ports::{
    ControlledExecutionPort, RemotePortFuture, RemoteSessionProjectionPort,
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
