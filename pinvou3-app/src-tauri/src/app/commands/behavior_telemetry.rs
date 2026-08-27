use super::prelude::*;

fn telemetry(
    app: &AppHandle,
) -> Result<tauri::State<'_, crate::features::behavior_telemetry::BehaviorTelemetry>, String> {
    app.try_state::<crate::features::behavior_telemetry::BehaviorTelemetry>()
        .ok_or_else(|| "behavior telemetry is unavailable".to_string())
}

#[tauri::command]
pub async fn get_behavior_telemetry_status(
    app: AppHandle,
) -> Result<crate::features::behavior_telemetry::BehaviorTelemetryStatus, String> {
    Ok(telemetry(&app)?.status().await)
}

#[tauri::command]
pub async fn set_behavior_telemetry_enabled(
    enabled: bool,
    app: AppHandle,
) -> Result<crate::features::behavior_telemetry::BehaviorTelemetryStatus, String> {
    telemetry(&app)?
        .set_collection_enabled(enabled)
        .await
        .map_err(|error| format!("update behavior telemetry consent: {error:#}"))
}

#[tauri::command]
pub async fn delete_local_behavior_telemetry_data(
    app: AppHandle,
) -> Result<crate::features::behavior_telemetry::BehaviorTelemetryStatus, String> {
    telemetry(&app)?
        .delete_local_data()
        .await
        .map_err(|error| format!("delete local behavior telemetry data: {error:#}"))
}

#[tauri::command]
pub async fn delete_remote_behavior_telemetry_data(
    app: AppHandle,
) -> Result<crate::features::behavior_telemetry::BehaviorTelemetryStatus, String> {
    telemetry(&app)?
        .delete_remote_data()
        .await
        .map_err(|error| format!("delete remote behavior telemetry data: {error:#}"))
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TrackBehaviorEventRequest {
    pub event_name: String,
    pub session_id: Option<String>,
    pub turn_id: Option<String>,
    pub input_type: Option<String>,
    pub status: Option<String>,
    pub stage: Option<String>,
    pub tool_key: Option<String>,
    pub tool_name: Option<String>,
    pub tool_type: Option<String>,
    pub success: Option<bool>,
    pub scene_l1: Option<String>,
    pub scene_l2: Option<String>,
    pub operation_id: Option<String>,
    pub attempt_index: Option<u16>,
    pub entry_point: Option<String>,
    pub duration_ms: Option<u64>,
    pub error_category: Option<String>,
    pub provider_key: Option<String>,
    pub provider_configured: Option<bool>,
    pub step: Option<String>,
    pub readiness_source: Option<String>,
    pub voice_permission_status: Option<String>,
    pub recognizer_type: Option<String>,
    pub view_id: Option<String>,
    pub position_bucket: Option<String>,
}

#[tauri::command]
pub fn track_behavior_event(
    request: TrackBehaviorEventRequest,
    app: AppHandle,
) -> Result<(), String> {
    let event_name = match request.event_name.as_str() {
        "voice_entry_clicked" => "voice_entry_clicked",
        "voice_permission_result" => "voice_permission_result",
        "voice_started" => "voice_started",
        "voice_recognition_completed" => "voice_recognition_completed",
        "voice_recognition_failed" => "voice_recognition_failed",
        "voice_cancelled" => "voice_cancelled",
        "voice_message_submitted" => {
            let session_id = request
                .session_id
                .as_deref()
                .ok_or_else(|| "voice_message_submitted requires session_id".to_string())?;
            let operation_id = request
                .operation_id
                .as_deref()
                .ok_or_else(|| "voice_message_submitted requires operation_id".to_string())?;
            return crate::features::behavior_telemetry::track_voice_message_submitted(
                &app,
                session_id,
                operation_id,
            );
        }
        "scene_submitted" => {
            let session_id = request
                .session_id
                .as_deref()
                .ok_or_else(|| "scene submission requires session_id".to_string())?;
            return crate::features::behavior_telemetry::track_scene_submitted(
                &app,
                session_id,
                request.scene_l1.as_deref().unwrap_or("unknown"),
                request.scene_l2.as_deref().unwrap_or("unknown"),
            );
        }
        "tool_impression" => "tool_impression",
        "tool_detail_opened" => "tool_detail_opened",
        "tool_configuration_started" => "tool_configuration_started",
        "tool_configuration_finished" => "tool_configuration_finished",
        "scene_impression" => "scene_impression",
        "scene_selected" => "scene_selected",
        "onboarding_started" => "onboarding_started",
        "onboarding_completed" => "onboarding_completed",
        "onboarding_skipped" => "onboarding_skipped",
        "provider_setup_started" => "provider_setup_started",
        "provider_setup_completed" => "provider_setup_completed",
        "provider_setup_failed" => "provider_setup_failed",
        "task_environment_ready" => "task_environment_ready",
        _ => return Err("unsupported behavior event".to_string()),
    };
    let mut event = crate::features::behavior_telemetry::BehaviorEvent::new(event_name);
    if let Some(value) = request.session_id {
        event = event.session(value);
    }
    if let Some(value) = request.turn_id {
        event = event.turn(value);
    }
    if let Some(value) = request.input_type {
        event = event.input_type(value);
    }
    if let Some(value) = request.status {
        event = event.status(value);
    }
    if let Some(value) = request.stage {
        event = event.stage(value);
    }
    if request.tool_key.is_some() || request.tool_name.is_some() || request.tool_type.is_some() {
        let raw_tool = request
            .tool_key
            .as_deref()
            .or(request.tool_name.as_deref())
            .unwrap_or_default();
        let tool_type = match request.tool_type.as_deref() {
            Some("mcp") => "mcp",
            Some("cli") => "cli",
            Some("skill") => "skill",
            Some("builtin") => "builtin",
            Some("custom") => "custom",
            _ => crate::features::behavior_telemetry::classify_tool(raw_tool),
        };
        event = event.tool(
            crate::features::behavior_telemetry::tool_registry_key(raw_tool),
            "",
            tool_type,
        );
    }
    if let Some(value) = request.success {
        event = event.success(value);
    }
    if request.scene_l1.is_some() || request.scene_l2.is_some() {
        let (scene_l1, scene_l2) = crate::features::behavior_telemetry::scene_registry_pair(
            request.scene_l1.as_deref().unwrap_or("unknown"),
            request.scene_l2.as_deref().unwrap_or("unknown"),
        );
        event = event.scene(scene_l1, scene_l2);
    }
    if let Some(value) = request.operation_id {
        event = event.string("operation_id", value);
    }
    if let Some(value) = request.attempt_index {
        event = event.integer("attempt_index", u64::from(value));
    }
    if let Some(value) = request.entry_point {
        event = event.string("entry_point", value);
    }
    if let Some(value) = request.duration_ms {
        event = event.integer("duration_ms", value.min(86_400_000));
    }
    if let Some(value) = request.error_category {
        event = event.string("error_category", value);
    }
    if let Some(value) = request.provider_key {
        event = event.string("provider_key", value);
    }
    if let Some(value) = request.provider_configured {
        event = event.boolean("provider_configured", value);
    }
    if let Some(value) = request.step {
        event = event.string("step", value);
    }
    if let Some(value) = request.readiness_source {
        event = event.string("readiness_source", value);
    }
    if let Some(value) = request.voice_permission_status {
        event = event.string("voice_permission_status", value);
    }
    if let Some(value) = request.recognizer_type {
        event = event.string("recognizer_type", value);
    }
    if let Some(value) = request.view_id {
        event = event.string("view_id", value);
    }
    if let Some(value) = request.position_bucket {
        event = event.string("position_bucket", value);
    }
    crate::features::behavior_telemetry::track(&app, event);
    Ok(())
}
