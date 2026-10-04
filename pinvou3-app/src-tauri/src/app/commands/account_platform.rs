use tauri::State;

use crate::features::account_platform::{
    AccountPlatformState, ApiRequest, ApiResponse, CallbackCopy, SessionStatus,
};

#[tauri::command]
pub async fn account_platform_session(
    state: State<'_, AccountPlatformState>,
) -> Result<SessionStatus, String> {
    Ok(state.status().await)
}

#[tauri::command]
pub async fn account_platform_login(
    callback_copy: CallbackCopy,
    state: State<'_, AccountPlatformState>,
) -> Result<SessionStatus, String> {
    state.login(callback_copy).await
}

#[tauri::command]
pub async fn account_platform_logout(
    state: State<'_, AccountPlatformState>,
) -> Result<SessionStatus, String> {
    state.logout().await
}

#[tauri::command]
pub async fn account_platform_request(
    request: ApiRequest,
    state: State<'_, AccountPlatformState>,
) -> Result<ApiResponse, String> {
    state.request(request).await
}
