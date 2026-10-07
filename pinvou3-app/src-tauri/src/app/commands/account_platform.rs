use tauri::State;

use crate::features::account_platform::{
    AccountPlatformState, ApiRequest, ApiResponse, LoginCredentials, MobileRegistration,
    MobileRegistrationPrecheck, PasswordReset, PortalOpenRequest, SMSLoginCredentials, SMSRequest,
    SessionStatus, SliderCaptchaRequest,
};

#[tauri::command]
pub async fn account_platform_session(
    state: State<'_, AccountPlatformState>,
) -> Result<SessionStatus, String> {
    Ok(state.status().await)
}

#[tauri::command]
pub async fn account_platform_open_portal(
    input: PortalOpenRequest,
    state: State<'_, AccountPlatformState>,
) -> Result<(), String> {
    state.open_portal(input.language).await
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
) -> Result<SessionStatus, String> {
    let status = state.login(credentials).await?;
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
) -> Result<SessionStatus, String> {
    let status = state.login_sms(input).await?;
    Ok(status)
}

#[tauri::command]
pub async fn account_platform_logout(
    state: State<'_, AccountPlatformState>,
) -> Result<SessionStatus, String> {
    let status = state.logout().await?;
    Ok(status)
}

#[tauri::command]
pub async fn account_platform_request(
    request: ApiRequest,
    state: State<'_, AccountPlatformState>,
) -> Result<ApiResponse, String> {
    state.request(request).await
}
