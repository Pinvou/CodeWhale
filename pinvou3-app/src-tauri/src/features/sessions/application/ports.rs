//! Shared, host-independent shapes for session application use cases.

use std::fmt;
use std::future::Future;
use std::pin::Pin;

use serde_json::Value;

/// Future returned by an application use-case boundary.
pub(crate) type UseCaseFuture<'a, T> =
    Pin<Box<dyn Future<Output = Result<T, UseCaseError>> + Send + 'a>>;

/// Stable error returned by session application boundaries.
///
/// The static code prevents arbitrary infrastructure details from becoming an
/// IPC or wire contract. Feature implementations retain the original error in
/// their own logs when appropriate.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct UseCaseError {
    code: &'static str,
    retryable: bool,
}

impl UseCaseError {
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

impl fmt::Display for UseCaseError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for UseCaseError {}

/// Host notification prepared only after the authoritative application state
/// has committed. It contains no Tauri handle or event emitter.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct ApplicationEvent {
    topic: &'static str,
    payload: Value,
}

impl ApplicationEvent {
    pub(crate) fn new(topic: &'static str, payload: Value) -> Self {
        Self { topic, payload }
    }

    pub(crate) const fn topic(&self) -> &'static str {
        self.topic
    }

    pub(crate) const fn payload(&self) -> &Value {
        &self.payload
    }
}

/// Narrow host event port injected by the composition root.
///
/// Implementations may adapt to the application's event bus, but application
/// use cases only depend on this synchronous notification contract.
pub(crate) trait ApplicationEventPort: Send + Sync {
    fn publish(&self, event: ApplicationEvent) -> Result<(), UseCaseError>;
}
