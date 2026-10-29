//! Compose model-store notifications with engine invalidation and app transports.

use crate::features::model_store::ModelsChangedHandler;
use tauri::{AppHandle, Emitter, Runtime};

/// Mark engine dependencies before either UI or remote consumers see the event.
/// The composition root supplies the existing live EnginePool lookup; keeping
/// that port separate also permits real command/service tests without a Wry UI.
pub(crate) fn models_changed_handler<R, F>(
    app: AppHandle<R>,
    mark_updated: F,
) -> ModelsChangedHandler
where
    R: Runtime,
    F: Fn(&[String]) + Send + Sync + 'static,
{
    std::sync::Arc::new(move |ids| {
        mark_updated(ids);
        let payload = serde_json::json!({ "model_ids": ids });
        let _ = app.emit("models:changed", &payload);
        crate::platform::app_events::forward_app_event(&app, "models:changed", payload);
    })
}

#[cfg(test)]
#[path = "model_store_events/platform/mod.rs"]
mod platform_tests;
