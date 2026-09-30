//! A 端远程会话与任务意图命令（E2E-03）。
//!
//! 前端只提交 UI 级字段（会话引用、标题、模式、正文），由 Rust 按冻结 schema
//! 构造 payload（含输入摘要）并返回；任何上下文不符（未连接、lease 变更、
//! 未取到能力快照、模式未开放）都失败关闭，绝不构造半合法请求。

use serde_json::Value;
use tauri::{AppHandle, Emitter, State};

use crate::features::remote_control::controller::controlling::{
    ControllingDeviceManager, ControllingStateError,
};
use crate::features::remote_control::controller::controlling_session::{
    ControllingIntentProjection, ControllingSessionManager, ControllingSessionProjection,
    SessionError,
};
use crate::features::remote_control::controller::encrypted_lane::ControllingLaneState;
use crate::features::remote_control::controller::relay_transport::ControllerSendError;

const UNSUPPORTED: &str = "desktop_remote_control_unsupported";
const SESSION_EVENT: &str = "desktop-remote-controller-session";
const DEFAULT_MODE: &str = "chat";

fn ensure_supported(manager: &ControllingDeviceManager) -> Result<(), String> {
    if manager.is_supported() {
        Ok(())
    } else {
        Err(UNSUPPORTED.to_owned())
    }
}

fn session_error(error: SessionError) -> String {
    error.code.to_owned()
}

fn controlling_error(error: ControllingStateError) -> String {
    error.code.to_owned()
}

/// 当前活租约与协商能力；未连接时失败关闭。
fn live_context(
    manager: &ControllingDeviceManager,
) -> Result<
    (
        crate::features::remote_control::controller::protocol::LeaseContext,
        Vec<String>,
    ),
    String,
> {
    manager.live_runtime_context().map_err(controlling_error)
}

fn emit_session(app: &AppHandle, projection: &ControllingSessionProjection) {
    let _ = app.emit(SESSION_EVENT, projection);
}

/// 出站失败收敛为有界错误码（不把内部描述文本透传给前端）。
fn send_error_code(error: ControllerSendError) -> String {
    match error {
        ControllerSendError::Unauthenticated => "controller_not_connected",
        ControllerSendError::FrameTooLarge { .. }
        | ControllerSendError::InvalidFrame(_)
        | ControllerSendError::Serialization => "task_request_invalid",
        ControllerSendError::ByteBudgetExhausted
        | ControllerSendError::QueueFull(_)
        | ControllerSendError::GenerationAlreadyBound
        | ControllerSendError::Closed => "controller_lane_unavailable",
    }
    .to_owned()
}

#[tauri::command]
pub fn desktop_remote_controller_session_open(
    app: AppHandle,
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    remote_ref: Option<String>,
    title: String,
    mode: Option<String>,
) -> Result<ControllingSessionProjection, String> {
    ensure_supported(&manager)?;
    let (lease, capabilities) = live_context(&manager)?;
    let projection = sessions
        .open(
            remote_ref.as_deref(),
            &title,
            mode.as_deref().unwrap_or(DEFAULT_MODE),
            &lease,
            &capabilities,
        )
        .map_err(session_error)?;
    emit_session(&app, &projection);
    Ok(projection)
}

#[tauri::command]
pub fn desktop_remote_controller_session_state(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    remote_ref: String,
) -> Result<ControllingSessionProjection, String> {
    ensure_supported(&manager)?;
    let (lease, _) = live_context(&manager)?;
    sessions
        .state(&remote_ref, &lease)
        .map_err(session_error)
}

#[tauri::command]
pub fn desktop_remote_controller_session_close(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    remote_ref: String,
) -> Result<(), String> {
    ensure_supported(&manager)?;
    sessions.close(&remote_ref).map_err(session_error)
}

/// 构造能力快照请求（A 端此后才能拿到 capability revision 并提交任务）。
#[tauri::command]
pub fn desktop_remote_controller_capability_request(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    remote_ref: String,
) -> Result<ControllingIntentProjection, String> {
    ensure_supported(&manager)?;
    let (lease, _) = live_context(&manager)?;
    sessions
        .capability_request(&remote_ref, &lease)
        .map_err(session_error)
}

/// 应用 B 回传的能力快照：记录 revision / 模式 / 工作区。
#[tauri::command]
pub fn desktop_remote_controller_apply_capability_snapshot(
    app: AppHandle,
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    remote_ref: String,
    payload: Value,
) -> Result<ControllingSessionProjection, String> {
    ensure_supported(&manager)?;
    let (lease, _) = live_context(&manager)?;
    let projection = sessions
        .apply_capability_snapshot(&remote_ref, &payload, &lease)
        .map_err(session_error)?;
    emit_session(&app, &projection);
    Ok(projection)
}

/// 构造任务提交意图：正文来自 UI，其余字段与输入摘要由 Rust 生成。
#[tauri::command]
pub fn desktop_remote_controller_task_intent(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    remote_ref: String,
    text: String,
) -> Result<ControllingIntentProjection, String> {
    ensure_supported(&manager)?;
    let (lease, _) = live_context(&manager)?;
    sessions
        .task_intent(&remote_ref, &text, &lease)
        .map_err(session_error)
}

/// 构造任务取消意图：只允许取消本会话已构造的任务。
#[tauri::command]
pub fn desktop_remote_controller_task_cancel_intent(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    remote_ref: String,
    task_id: String,
) -> Result<ControllingIntentProjection, String> {
    ensure_supported(&manager)?;
    let (lease, _) = live_context(&manager)?;
    sessions
        .task_cancel_intent(&remote_ref, &task_id, &lease)
        .map_err(session_error)
}

/// 发送能力快照请求：构造 → 加密 → 经认证 transport 发出（E2E-04 补链）。
/// A 端必须先拿到 capability revision，任务意图才被允许构造。
#[tauri::command]
pub fn desktop_remote_controller_capability_send(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    business: State<'_, ControllingLaneState>,
    remote_ref: String,
) -> Result<ControllingIntentProjection, String> {
    ensure_supported(&manager)?;
    let (lease, capabilities) = live_context(&manager)?;
    let intent = sessions
        .capability_request(&remote_ref, &lease)
        .map_err(session_error)?;
    let frame = business
        .seal("capability_snapshot_request", &intent.payload, &capabilities)
        .map_err(|error| error.code().to_owned())?;
    manager
        .send_authenticated_business(&frame)
        .map_err(send_error_code)?;
    Ok(intent)
}

/// 发送任务：构造意图 → 加密 → 经认证 transport 发出（E2E-04）。
#[tauri::command]
pub fn desktop_remote_controller_task_send(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    business: State<'_, ControllingLaneState>,
    remote_ref: String,
    text: String,
) -> Result<ControllingIntentProjection, String> {
    ensure_supported(&manager)?;
    let (lease, capabilities) = live_context(&manager)?;
    let intent = sessions
        .task_intent(&remote_ref, &text, &lease)
        .map_err(session_error)?;
    let frame = business
        .seal("task_submit", &intent.payload, &capabilities)
        .map_err(|error| error.code().to_owned())?;
    manager
        .send_authenticated_business(&frame)
        .map_err(send_error_code)?;
    Ok(intent)
}

/// 发送取消：构造取消意图 → 加密 → 经认证 transport 发出（E2E-04）。
#[tauri::command]
pub fn desktop_remote_controller_task_cancel_send(
    manager: State<'_, ControllingDeviceManager>,
    sessions: State<'_, ControllingSessionManager>,
    business: State<'_, ControllingLaneState>,
    remote_ref: String,
    task_id: String,
) -> Result<ControllingIntentProjection, String> {
    ensure_supported(&manager)?;
    let (lease, capabilities) = live_context(&manager)?;
    let intent = sessions
        .task_cancel_intent(&remote_ref, &task_id, &lease)
        .map_err(session_error)?;
    let frame = business
        .seal("task_cancel", &intent.payload, &capabilities)
        .map_err(|error| error.code().to_owned())?;
    manager
        .send_authenticated_business(&frame)
        .map_err(send_error_code)?;
    Ok(intent)
}

#[cfg(test)]
mod tests {
    #[test]
    fn session_command_surface_is_bounded() {
        assert_eq!(super::SESSION_EVENT, "desktop-remote-controller-session");
        assert_eq!(super::UNSUPPORTED, "desktop_remote_control_unsupported");
        assert_eq!(super::DEFAULT_MODE, "chat");
    }
}
