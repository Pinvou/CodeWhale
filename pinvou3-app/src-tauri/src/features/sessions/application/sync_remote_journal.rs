//! Boundary reserved for synchronising authoritative remote journal entries.

use super::ports::UseCaseFuture;

pub(crate) trait SyncRemoteJournalUseCase: Send + Sync {
    type Request: Send + 'static;
    type Response: Send + 'static;

    fn sync_remote_journal(&self, request: Self::Request) -> UseCaseFuture<'_, Self::Response>;
}
