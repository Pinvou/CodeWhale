//! Dependency-inversion ports for authenticated remote-control ingress.
//!
//! The remote-control feature owns these ports, while the application
//! composition root supplies stateless adapters. Request and response types
//! remain associated types so the later typed-envelope and controlled-submit
//! tasks can define their domain contracts without making this feature depend
//! on `sessions`, `app`, or Tauri.

use std::fmt;
use std::future::Future;
use std::pin::Pin;

/// Boxed future used by remote-control ingress ports.
pub(crate) type RemotePortFuture<'a, T> =
    Pin<Box<dyn Future<Output = Result<T, RemotePortError>> + Send + 'a>>;

/// Stable failure shape returned across the remote-control/application seam.
///
/// Codes are bounded static identifiers rather than transport error strings;
/// callers may safely map them to protocol errors without leaking internals.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct RemotePortError {
    code: &'static str,
    retryable: bool,
}

impl RemotePortError {
    pub(crate) const fn new(code: &'static str, retryable: bool) -> Self {
        Self { code, retryable }
    }

    pub(crate) const fn code(self) -> &'static str {
        self.code
    }

    pub(crate) const fn is_retryable(self) -> bool {
        self.retryable
    }
}

impl fmt::Display for RemotePortError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for RemotePortError {}

/// B-side entry point after transport authentication, decryption and replay
/// validation have succeeded.
///
/// The adapter must delegate to the same Tauri-independent use case used by a
/// local controlled-device input. The port deliberately owns no session or
/// task state.
pub(crate) trait ControlledExecutionPort: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn execute_controlled(&self, request: Self::Request) -> RemotePortFuture<'_, Self::Response>;
}

/// A-side entry point for an already authenticated remote-session event.
///
/// Event typing and binding validation are supplied by the later projection
/// task. This seam only fixes the dependency direction: remote control invokes
/// an injected adapter and never reaches into Commands or host event APIs.
pub(crate) trait RemoteSessionProjectionPort: Send + Sync {
    type Event: Send + 'static;
    type Projection: Send + 'static;

    fn project_authenticated(&self, event: Self::Event) -> RemotePortFuture<'_, Self::Projection>;
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    struct ControlledFake;

    impl ControlledExecutionPort for ControlledFake {
        type Request = u64;
        type Response = u64;

        fn execute_controlled(
            &self,
            request: Self::Request,
        ) -> RemotePortFuture<'_, Self::Response> {
            Box::pin(async move { Ok(request + 1) })
        }
    }

    struct ProjectionFake;

    impl RemoteSessionProjectionPort for ProjectionFake {
        type Event = &'static str;
        type Projection = usize;

        fn project_authenticated(
            &self,
            event: Self::Event,
        ) -> RemotePortFuture<'_, Self::Projection> {
            Box::pin(async move { Ok(event.len()) })
        }
    }

    #[tokio::test]
    async fn ports_can_be_implemented_without_a_tauri_host() {
        fn assert_send_sync<T: Send + Sync + ?Sized>(_: &T) {}
        fn assert_send_future<F: Future + Send>(_: &F) {}

        let controlled: Arc<dyn ControlledExecutionPort<Request = u64, Response = u64>> =
            Arc::new(ControlledFake);
        let projection: Arc<
            dyn RemoteSessionProjectionPort<Event = &'static str, Projection = usize>,
        > = Arc::new(ProjectionFake);
        assert_send_sync(controlled.as_ref());
        assert_send_sync(projection.as_ref());

        let execution = controlled.execute_controlled(7);
        assert_send_future(&execution);
        assert_eq!(execution.await.unwrap(), 8);
        let projected = projection.project_authenticated("event");
        assert_send_future(&projected);
        assert_eq!(projected.await.unwrap(), 5);

        let error = RemotePortError::new("session_busy", true);
        assert_eq!(error.code(), "session_busy");
        assert!(error.is_retryable());
    }
}
