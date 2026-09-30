//! Boundary reserved for B-side local and authenticated remote submissions.

use super::ports::UseCaseFuture;

pub(crate) trait SubmitControlledUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn submit(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}
