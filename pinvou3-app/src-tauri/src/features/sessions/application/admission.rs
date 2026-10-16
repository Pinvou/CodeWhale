//! Admission boundary reserved for the shared local/controlled turn policy.

use super::ports::UseCaseFuture;

pub(crate) trait AdmissionUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn admit(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}
