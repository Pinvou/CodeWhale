use crate::features::team_relay::{
    AccountEnrollmentInput, Attachment, ClaimEnrollmentInput, CreateActionInput,
    CreateInviteResponse, CreateTeamEnrollmentInput, CreateThreadInput, MarkThreadReadInput,
    SetTeamRelayAgentSessionInput, SetThreadPreferenceInput, TeamJoinCodeState,
    TeamRelayAccountState, TeamRelayLifecycleResult, TeamRelayManager, TeamRelaySnapshot,
    TeamRelayStatus, TeamRelayTaskWorkspaceStatus, ThreadMutationResponse, ThreadPreference,
    ThreadReadState, UpdateProfileInput,
};
use std::path::PathBuf;
use tauri::{AppHandle, State};

#[tauri::command]
pub fn team_relay_status(manager: State<'_, TeamRelayManager>) -> Result<TeamRelayStatus, String> {
    manager.status()
}

#[tauri::command]
pub async fn team_relay_verify_beta_access(
    access_code: String,
    manager: State<'_, TeamRelayManager>,
) -> Result<bool, String> {
    manager.verify_beta_access(access_code).await
}

#[tauri::command]
pub async fn team_relay_register_account(
    input: AccountEnrollmentInput,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelayAccountState, String> {
    manager.register_account(input).await
}

#[tauri::command]
pub async fn team_relay_account(
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelayAccountState, String> {
    manager.account_state().await
}

#[tauri::command]
pub async fn team_relay_claim(
    input: ClaimEnrollmentInput,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    manager.claim(input).await
}

#[tauri::command]
pub async fn team_relay_create_first_team(
    input: CreateTeamEnrollmentInput,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    manager.create_first_team(input).await
}

#[tauri::command]
pub async fn team_relay_bootstrap(
    app: AppHandle,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    let snapshot = manager.bootstrap().await?;
    manager.start_events(
        app,
        &snapshot
            .teams
            .iter()
            .map(|team| team.id.clone())
            .collect::<Vec<_>>(),
    )?;
    Ok(snapshot)
}

#[tauri::command]
pub async fn team_relay_update_profile(
    input: UpdateProfileInput,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    manager.update_profile(input).await
}

#[tauri::command]
pub async fn team_relay_upload_avatar(
    path: PathBuf,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    manager.upload_avatar(&path).await
}

#[tauri::command]
pub async fn team_relay_remove_avatar(
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    manager.remove_avatar().await
}

#[tauri::command]
pub async fn team_relay_avatar_data_url(
    team_id: Option<String>,
    principal_id: String,
    expected_sha256: String,
    display_size: Option<u16>,
    reduced_motion: Option<bool>,
    manager: State<'_, TeamRelayManager>,
) -> Result<Option<String>, String> {
    manager
        .avatar_data_url(
            team_id.as_deref(),
            &principal_id,
            &expected_sha256,
            display_size,
            reduced_motion.unwrap_or(false),
        )
        .await
}

#[tauri::command]
pub async fn team_relay_create_team(
    app: AppHandle,
    name: String,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    let snapshot = manager.create_team(&name).await?;
    manager.start_events(
        app,
        &snapshot
            .teams
            .iter()
            .map(|team| team.id.clone())
            .collect::<Vec<_>>(),
    )?;
    Ok(snapshot)
}

#[tauri::command]
pub async fn team_relay_join_team(
    app: AppHandle,
    join_code: String,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    let snapshot = manager.join_team(&join_code).await?;
    manager.start_events(
        app,
        &snapshot
            .teams
            .iter()
            .map(|team| team.id.clone())
            .collect::<Vec<_>>(),
    )?;
    Ok(snapshot)
}

#[tauri::command]
pub async fn team_relay_switch_team(
    app: AppHandle,
    team_id: String,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelaySnapshot, String> {
    let snapshot = manager.switch_team(&team_id).await?;
    manager.start_events(
        app,
        &snapshot
            .teams
            .iter()
            .map(|team| team.id.clone())
            .collect::<Vec<_>>(),
    )?;
    Ok(snapshot)
}

#[tauri::command]
pub async fn team_relay_create_invite(
    expires_in_hours: u16,
    manager: State<'_, TeamRelayManager>,
) -> Result<CreateInviteResponse, String> {
    manager.create_invite(expires_in_hours).await
}

#[tauri::command]
pub async fn team_relay_get_join_password(
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamJoinCodeState, String> {
    manager.get_join_password().await
}

#[tauri::command]
pub async fn team_relay_rotate_join_password(
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamJoinCodeState, String> {
    manager.rotate_join_password().await
}

#[tauri::command]
pub async fn team_relay_set_join_password_visibility(
    visibility: String,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamJoinCodeState, String> {
    manager.set_join_password_visibility(&visibility).await
}

#[tauri::command]
pub async fn team_relay_disable_join_password(
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamJoinCodeState, String> {
    manager.disable_join_password().await
}

#[tauri::command]
pub async fn team_relay_remove_member(
    principal_id: String,
    manager: State<'_, TeamRelayManager>,
) -> Result<(), String> {
    manager.remove_member(&principal_id).await
}

fn restart_lifecycle_events(
    app: AppHandle,
    manager: &TeamRelayManager,
    result: &mut TeamRelayLifecycleResult,
) {
    if let Some(account) = &result.account {
        let team_ids = account
            .teams
            .iter()
            .map(|team| team.id.clone())
            .collect::<Vec<_>>();
        // start_events errors are deliberately degraded to a sync warning instead of being
        // propagated with ?, because the server already committed the lifecycle mutation.
        let outcome = if team_ids.is_empty() {
            // The last team was left or dissolved: only stop the previous event task instead
            // of starting an empty-scope SSE loop that would reconnect every 3 seconds.
            manager.stop_events()
        } else {
            manager.start_events(app, &team_ids)
        };
        if let Err(error) = outcome {
            result.record_sync_error(error);
        }
    }
}

#[tauri::command]
pub async fn team_relay_transfer_owner(
    app: AppHandle,
    new_owner_id: String,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelayLifecycleResult, String> {
    let mut result = manager.transfer_owner(&new_owner_id).await?;
    restart_lifecycle_events(app, &manager, &mut result);
    Ok(result)
}

#[tauri::command]
pub async fn team_relay_leave_team(
    app: AppHandle,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelayLifecycleResult, String> {
    let mut result = manager.leave_team().await?;
    restart_lifecycle_events(app, &manager, &mut result);
    Ok(result)
}

#[tauri::command]
pub async fn team_relay_dissolve_team(
    app: AppHandle,
    manager: State<'_, TeamRelayManager>,
) -> Result<TeamRelayLifecycleResult, String> {
    let mut result = manager.dissolve_team().await?;
    restart_lifecycle_events(app, &manager, &mut result);
    Ok(result)
}

#[tauri::command]
pub async fn team_relay_create_thread(
    input: CreateThreadInput,
    manager: State<'_, TeamRelayManager>,
) -> Result<ThreadMutationResponse, String> {
    manager.create_thread(input).await
}

#[tauri::command]
pub async fn team_relay_create_action(
    thread_id: String,
    input: CreateActionInput,
    manager: State<'_, TeamRelayManager>,
) -> Result<ThreadMutationResponse, String> {
    manager.create_action(&thread_id, input).await
}

#[tauri::command]
pub async fn team_relay_set_thread_preference(
    thread_id: String,
    input: SetThreadPreferenceInput,
    manager: State<'_, TeamRelayManager>,
) -> Result<ThreadPreference, String> {
    manager.set_thread_preference(&thread_id, input).await
}

#[tauri::command]
pub async fn team_relay_mark_thread_read(
    thread_id: String,
    input: MarkThreadReadInput,
    manager: State<'_, TeamRelayManager>,
) -> Result<ThreadReadState, String> {
    manager.mark_thread_read(&thread_id, input).await
}

#[tauri::command]
pub async fn team_relay_upload_attachment(
    app: AppHandle,
    path: PathBuf,
    upload_id: String,
    team_id: String,
    manager: State<'_, TeamRelayManager>,
) -> Result<Attachment, String> {
    manager
        .upload_attachment(app, &path, &upload_id, &team_id)
        .await
}

#[tauri::command]
pub async fn team_relay_download_attachment(
    attachment_id: String,
    destination: PathBuf,
    manager: State<'_, TeamRelayManager>,
) -> Result<(), String> {
    manager
        .download_attachment(&attachment_id, &destination)
        .await
}

#[tauri::command]
pub fn team_relay_task_workspace_status(
    thread_id: String,
    team_id: Option<String>,
    manager: State<'_, TeamRelayManager>,
    app: AppHandle,
) -> Result<TeamRelayTaskWorkspaceStatus, String> {
    let status = manager.task_workspace_status(&thread_id, team_id.as_deref())?;
    if let Some(link) = status.agent_session.as_ref() {
        crate::features::behavior_telemetry::register_team_session(&app, &link.session_id);
    }
    Ok(status)
}

#[tauri::command]
pub async fn team_relay_prepare_task_workspace(
    thread_id: String,
    manager: State<'_, TeamRelayManager>,
    app: AppHandle,
) -> Result<TeamRelayTaskWorkspaceStatus, String> {
    let status = manager.prepare_task_workspace(&thread_id).await?;
    if let Some(link) = status.agent_session.as_ref() {
        crate::features::behavior_telemetry::register_team_session(&app, &link.session_id);
    }
    Ok(status)
}

#[tauri::command]
pub fn team_relay_reveal_prepared_material(
    thread_id: String,
    attachment_id: String,
    team_id: Option<String>,
    manager: State<'_, TeamRelayManager>,
) -> Result<(), String> {
    manager.reveal_prepared_material(&thread_id, &attachment_id, team_id.as_deref())
}

#[tauri::command]
pub fn team_relay_set_task_agent_session(
    thread_id: String,
    input: SetTeamRelayAgentSessionInput,
    team_id: Option<String>,
    manager: State<'_, TeamRelayManager>,
    app: AppHandle,
) -> Result<TeamRelayTaskWorkspaceStatus, String> {
    let session_id = input.session_id.clone();
    let status = manager.set_task_agent_session(&thread_id, input, team_id.as_deref())?;
    crate::features::behavior_telemetry::register_team_session(&app, &session_id);
    Ok(status)
}

#[tauri::command]
pub fn team_relay_disconnect(manager: State<'_, TeamRelayManager>) -> Result<(), String> {
    manager.disconnect()
}
