use tauri::State;

use crate::features::account_platform::{
    AccountPlatformState, ApiRequest, ApiResponse, LoginCredentials, MobileRegistration,
    MobileRegistrationPrecheck, PasswordReset, SMSLoginCredentials, SMSRequest, SessionStatus,
    SliderCaptchaRequest,
};
use crate::features::assistant::engine_pool::EnginePool;
use crate::platform::prefs::BUILTIN_ACCOUNT_PLATFORM_MODEL_ID;

#[tauri::command]
pub async fn account_platform_session(
    state: State<'_, AccountPlatformState>,
) -> Result<SessionStatus, String> {
    Ok(state.status().await)
}

#[tauri::command]
pub async fn account_platform_reset_password(
    input: PasswordReset,
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<serde_json::Value, String> {
    let result = state.reset_password(input).await?;
    pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    Ok(result)
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
    pool: State<'_, EnginePool>,
) -> Result<ApiResponse, String> {
    let completes_profile =
        crate::features::account_platform::profile_completion_requested(&request);
    let changes_context =
        crate::features::account_platform::active_context_change_requested(&request);
    let previous_context_revision = state.context_revision();
    let response = state.request(request).await?;
    if (changes_context && (200..300).contains(&response.status))
        || previous_context_revision != state.context_revision()
    {
        pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    }
    if completes_profile
        && (200..300).contains(&response.status)
        && response
            .body
            .get("status")
            .and_then(serde_json::Value::as_str)
            == Some("COMPLETED")
    {
        pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    }
    Ok(response)
}

#[tauri::command]
pub async fn account_platform_models(
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<crate::features::account_platform::BuiltinModelsView, String> {
    let previous_context_revision = state.context_revision();
    let result = state.builtin_models().await;
    if previous_context_revision != state.context_revision() {
        pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    }
    result
}

#[tauri::command]
pub async fn account_platform_set_default_model(
    model: String,
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<crate::features::account_platform::BuiltinModelsView, String> {
    let result = state.set_builtin_default_model(model).await?;
    pool.mark_model_updated(BUILTIN_ACCOUNT_PLATFORM_MODEL_ID);
    Ok(result)
}
