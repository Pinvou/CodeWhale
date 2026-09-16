//! Stable extension boundary between controller transport and task execution.

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::Value;
use tokio_util::sync::CancellationToken;

pub(crate) trait ControllerTaskEventSink: Send + Sync {
    fn emit(&self, event: Value);
}

#[async_trait]
pub(crate) trait ControllerTaskExecutor: Send + Sync {
    async fn execute(
        &self,
        request: Value,
        events: Arc<dyn ControllerTaskEventSink>,
        cancel: CancellationToken,
    ) -> Result<(), String>;
}
