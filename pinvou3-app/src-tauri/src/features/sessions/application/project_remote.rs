//! Boundary reserved for authenticated A-side remote-session projection.

use super::ports::UseCaseFuture;

pub(crate) trait ProjectRemoteUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn project(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}
