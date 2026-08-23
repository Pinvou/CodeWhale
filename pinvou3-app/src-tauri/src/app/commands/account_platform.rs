use tauri::{AppHandle, State};

use crate::features::account_platform::{
    AccountPlatformState, ApiRequest, ApiResponse, LoginCredentials, MobileRegistration,
    MobileRegistrationPrecheck, PasswordReset, SMSLoginCredentials, SMSRequest, SessionStatus,
    SliderCaptchaRequest,
};
use crate::features::assistant::engine_pool::EnginePool;
use crate::features::sessions::SessionStore;
use crate::platform::prefs::{UserPrefs, BUILTIN_ACCOUNT_PLATFORM_MODEL_ID};

#[tauri::command]
pub async fn account_platform_session(
    state: State<'_, AccountPlatformState>,
) -> Result<SessionStatus, String> {
    Ok(state.status().await)
}

#[tauri::command]
pub fn account_platform_open_portal() -> Result<(), String> {
    crate::features::account_platform::open_account_portal()
}

#[tauri::command]
pub async fn account_platform_reset_password(
    input: PasswordReset,
    state: State<'_, AccountPlatformState>,
) -> Result<serde_json::Value, String> {
    state.reset_password(input).await
}

#[tauri::command]
pub async fn account_platform_login(
    credentials: LoginCredentials,
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<SessionStatus, String> {
    let status = state.login(credentials).await?;
    pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    Ok(status)
}

#[tauri::command]
pub async fn account_platform_send_sms(
    input: SMSRequest,
    state: State<'_, AccountPlatformState>,
) -> Result<serde_json::Value, String> {
    state.send_sms(input).await
}

#[tauri::command]
pub async fn account_platform_create_slider_captcha(
    input: SliderCaptchaRequest,
    state: State<'_, AccountPlatformState>,
) -> Result<serde_json::Value, String> {
    state.create_slider_captcha(input).await
}

#[tauri::command]
pub async fn account_platform_precheck_mobile_registration(
    input: MobileRegistrationPrecheck,
    state: State<'_, AccountPlatformState>,
) -> Result<serde_json::Value, String> {
    state.precheck_mobile_registration(input).await
}

#[tauri::command]
pub async fn account_platform_register_mobile(
    input: MobileRegistration,
    state: State<'_, AccountPlatformState>,
) -> Result<serde_json::Value, String> {
    state.register_mobile(input).await
}

#[tauri::command]
pub async fn account_platform_registration_status(
    state: State<'_, AccountPlatformState>,
) -> Result<serde_json::Value, String> {
    state.registration_status().await
}

#[tauri::command]
pub async fn account_platform_login_sms(
    input: SMSLoginCredentials,
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<SessionStatus, String> {
    let status = state.login_sms(input).await?;
    pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    Ok(status)
}

#[tauri::command]
pub async fn account_platform_logout(
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<SessionStatus, String> {
    let status = state.logout().await?;
    pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    Ok(status)
}

#[tauri::command]
pub async fn account_platform_request(
    request: ApiRequest,
    state: State<'_, AccountPlatformState>,
) -> Result<ApiResponse, String> {
    state.request(request).await
}

#[tauri::command]
pub async fn account_platform_model_catalog(
    state: State<'_, AccountPlatformState>,
) -> Result<serde_json::Value, String> {
    let response = state
        .request(ApiRequest {
            area: "token".to_string(),
            resource: "/v1/models".to_string(),
            method: "GET".to_string(),
            body: None,
            idempotency_key: None,
        })
        .await?;
    if !(200..300).contains(&response.status) {
        return Err("model_catalog_unavailable: current catalog cannot be loaded".to_string());
    }
    let mut body = response.body;
    if let Some(object) = body.as_object_mut() {
        object.insert(
            "selected_model_id".to_string(),
            UserPrefs::load()
                .advanced
                .account_platform_default_model
                .map(serde_json::Value::String)
                .unwrap_or(serde_json::Value::Null),
        );
    }
    Ok(body)
}

#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CatalogModelSelection {
    model_id: String,
}

#[tauri::command]
pub async fn account_platform_select_catalog_model(
    input: CatalogModelSelection,
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<serde_json::Value, String> {
    select_catalog_model(&input.model_id, &state, &pool).await
}

async fn select_catalog_model(
    requested_model_id: &str,
    state: &AccountPlatformState,
    pool: &EnginePool,
) -> Result<serde_json::Value, String> {
    let model_id = requested_model_id.trim();
    if model_id.is_empty() || model_id.len() > 128 {
        return Err("model_catalog_selection_invalid: model_id is invalid".to_string());
    }
    let catalog = state
        .request(ApiRequest {
            area: "token".to_string(),
            resource: "/v1/models".to_string(),
            method: "GET".to_string(),
            body: None,
            idempotency_key: None,
        })
        .await?;
    if !(200..300).contains(&catalog.status) {
        return Err("model_catalog_unavailable: current catalog cannot be loaded".to_string());
    }
    let item = catalog
        .body
        .get("items")
        .and_then(serde_json::Value::as_array)
        .and_then(|items| {
            items.iter().find(|item| {
                item.get("model_id").and_then(serde_json::Value::as_str) == Some(model_id)
            })
        })
        .ok_or_else(|| {
            "model_catalog_selection_invalid: model is not catalog visible".to_string()
        })?;
    ensure_new_session_model_selectable(item)?;
    UserPrefs::update_transaction(|prefs| {
        prefs.advanced.account_platform_default_model = Some(model_id.to_string());
        let model = prefs
            .advanced
            .saved_models
            .iter_mut()
            .find(|model| model.id == BUILTIN_ACCOUNT_PLATFORM_MODEL_ID)
            .ok_or_else(|| "built-in account platform model is unavailable".to_string())?;
        model.model = model_id.to_string();
        Ok(())
    })?;
    pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    Ok(serde_json::json!({
        "model_id": model_id,
        "catalog_version": catalog.body.get("catalog_version"),
        "lifecycle_status": item.get("lifecycle_status"),
    }))
}

#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ReplacementSessionBranch {
    source_conversation_id: String,
    source_turn_id: String,
    source_message_count: usize,
    replacement_model_id: String,
}

#[tauri::command]
pub async fn account_platform_branch_replacement_session(
    input: ReplacementSessionBranch,
    app: AppHandle,
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
    store: State<'_, SessionStore>,
) -> Result<serde_json::Value, String> {
    if input.source_turn_id.trim().is_empty() || input.source_turn_id.len() > 160 {
        return Err("replacement_branch_invalid: source turn is required".to_string());
    }
    select_catalog_model(&input.replacement_model_id, &state, &pool).await?;
    let (session, lineage) = store
        .create_replacement_branch(
            input.source_conversation_id.trim(),
            input.source_message_count,
            input.source_turn_id.trim().to_string(),
            input.replacement_model_id.trim().to_string(),
        )
        .map_err(|error| format!("replacement_branch_failed: {error:#}"))?;
    store.set_active(Some(session.metadata.id.clone()));
    super::sessions::emit_session_event(
        &app,
        "session:list_changed",
        &session.metadata.id,
        "replacement_branch",
    );
    Ok(serde_json::json!({ "session": session.metadata, "lineage": lineage }))
}

fn ensure_new_session_model_selectable(item: &serde_json::Value) -> Result<(), String> {
    if item
        .get("context_eligible")
        .and_then(serde_json::Value::as_bool)
        != Some(true)
    {
        let reason = item
            .get("eligibility_reason")
            .and_then(serde_json::Value::as_str)
            .unwrap_or("MODEL_CONTEXT_INELIGIBLE");
        return Err(format!("model_catalog_selection_unavailable: {reason}"));
    }
    match item
        .get("lifecycle_status")
        .and_then(serde_json::Value::as_str)
    {
        Some("ACTIVE") => Ok(()),
        Some("DEPRECATED") => {
            let cutoff = item
                .get("new_session_cutoff_at")
                .and_then(serde_json::Value::as_str)
                .and_then(|value| chrono::DateTime::parse_from_rfc3339(value).ok());
            if cutoff.is_some_and(|value| value > chrono::Utc::now()) {
                Ok(())
            } else {
                Err("model_catalog_selection_unavailable: NEW_SESSION_CUTOFF_REACHED".to_string())
            }
        }
        _ => Err("model_catalog_selection_unavailable: MODEL_REMOVED".to_string()),
    }
}

#[cfg(test)]
mod tests {
    use super::ensure_new_session_model_selectable;

    #[test]
    fn catalog_selection_obeys_new_session_cutoff_and_context_eligibility() {
        assert!(ensure_new_session_model_selectable(&serde_json::json!({
            "lifecycle_status":"ACTIVE", "context_eligible":true
        }))
        .is_ok());
        assert!(ensure_new_session_model_selectable(&serde_json::json!({
            "lifecycle_status":"ACTIVE", "context_eligible":false,
            "eligibility_reason":"ORGANIZATION_MODEL_DENIED"
        }))
        .is_err());
        assert!(ensure_new_session_model_selectable(&serde_json::json!({
            "lifecycle_status":"DEPRECATED", "context_eligible":true,
            "new_session_cutoff_at":(chrono::Utc::now()+chrono::Duration::minutes(5)).to_rfc3339()
        }))
        .is_ok());
        assert!(ensure_new_session_model_selectable(&serde_json::json!({
            "lifecycle_status":"DEPRECATED", "context_eligible":true
        }))
        .is_err());
        assert!(ensure_new_session_model_selectable(&serde_json::json!({
            "lifecycle_status":"REMOVED", "context_eligible":true
        }))
        .is_err());
    }
}
