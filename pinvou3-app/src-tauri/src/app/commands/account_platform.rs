use tauri::State;

use crate::features::account_platform::{
    AccountPlatformState, ApiRequest, ApiResponse, LoginCredentials, SessionStatus,
};
use crate::features::assistant::engine_pool::EnginePool;
use crate::platform::prefs::BUILTIN_LLMAPI_MODEL_ID;

#[tauri::command]
pub async fn account_platform_session(
    state: State<'_, AccountPlatformState>,
) -> Result<SessionStatus, String> {
    Ok(state.status().await)
}

#[tauri::command]
pub async fn account_platform_login(
    credentials: LoginCredentials,
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<SessionStatus, String> {
    let status = state.login(credentials).await?;
    pool.mark_model_updated(BUILTIN_LLMAPI_MODEL_ID);
    Ok(status)
}

#[tauri::command]
pub async fn account_platform_logout(
    state: State<'_, AccountPlatformState>,
    pool: State<'_, EnginePool>,
) -> Result<SessionStatus, String> {
    let status = state.logout().await?;
    pool.mark_model_updated(BUILTIN_LLMAPI_MODEL_ID);
    Ok(status)
}

#[tauri::command]
pub async fn account_platform_request(
    request: ApiRequest,
    state: State<'_, AccountPlatformState>,
) -> Result<ApiResponse, String> {
    state.request(request).await
}
