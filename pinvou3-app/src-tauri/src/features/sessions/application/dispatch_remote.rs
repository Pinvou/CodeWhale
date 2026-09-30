//! Boundary reserved for A-side prepare, persist and remote dispatch.

use super::ports::UseCaseFuture;

pub(crate) trait DispatchRemoteUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn dispatch(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}
