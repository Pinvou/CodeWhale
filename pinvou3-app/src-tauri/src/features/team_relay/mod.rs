mod types;

pub use types::*;

use crate::platform::credential_store::{
    CredentialReference, CredentialStore, SystemCredentialStore,
};
use chrono::Utc;
use futures_util::{future::join_all, StreamExt};
use reqwest::{
    header::{HeaderMap, HeaderValue},
    multipart, Client, Response, StatusCode, Url,
};
use serde::de::DeserializeOwned;
use serde_json::json;
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::Duration;
use tauri::{AppHandle, Emitter};
use tokio::io::AsyncWriteExt;
use tokio_util::io::ReaderStream;

const MAX_ATTACHMENT_BYTES: u64 = 20 * 1024 * 1024;
const DEFAULT_TEAM_RELAY_HUB_URL: &str = "https://pinvou.com/pinvou3/team-relay";

#[derive(Debug, serde::Deserialize)]
struct HubErrorEnvelope {
    error: HubError,
}

#[derive(Debug, serde::Deserialize)]
struct HubError {
    code: String,
    message: String,
    request_id: String,
}

#[derive(Debug, serde::Deserialize)]
struct GetMeResponse {
    principal: Principal,
}

#[derive(Debug)]
pub struct TeamRelayManager {
    client: Client,
    event_client: Client,
    credentials: SystemCredentialStore,
    settings_path: PathBuf,
    event_task: Mutex<Option<TeamRelayEventTask>>,
}

#[derive(Debug)]
struct TeamRelayEventTask {
    scope_key: String,
    handle: tauri::async_runtime::JoinHandle<()>,
}

impl TeamRelayManager {
    pub fn new() -> Result<Self, String> {
        let mut headers = HeaderMap::new();
        headers.insert(
            "x-pinvou-team-relay-protocol",
            HeaderValue::from_static("0.2"),
        );
        let client = Client::builder()
            .connect_timeout(Duration::from_secs(10))
            .timeout(Duration::from_secs(45))
            .user_agent(concat!("Pinvou-Team-Relay/", env!("CARGO_PKG_VERSION")))
            .default_headers(headers.clone())
            .build()
            .map_err(|error| format!("team_relay_client_unavailable: {error}"))?;
        // SSE is an intentionally long-lived response. A total request timeout would
        // periodically tear it down and create a window in which change hints are lost.
        let event_client = Client::builder()
            .connect_timeout(Duration::from_secs(10))
            .user_agent(concat!("Pinvou-Team-Relay/", env!("CARGO_PKG_VERSION")))
            .default_headers(headers)
            .build()
            .map_err(|error| format!("team_relay_event_client_unavailable: {error}"))?;
        Ok(Self {
            client,
            event_client,
            credentials: SystemCredentialStore::new(),
            settings_path: crate::platform::paths::pinvou3_home()
                .join("team-relay")
                .join("settings.json"),
            event_task: Mutex::new(None),
        })
    }

    pub fn status(&self) -> Result<TeamRelayStatus, String> {
        let Some(settings) = self.load_settings()? else {
            return Ok(TeamRelayStatus {
                configured: false,
                hub_url: None,
                team_id: None,
            });
        };
        let configured = self.token(&settings)?.is_some();
        Ok(TeamRelayStatus {
            configured,
            hub_url: Some(settings.hub_url),
            team_id: if settings.team_id.is_empty() {
                None
            } else {
                Some(settings.team_id)
            },
        })
    }

    pub async fn verify_beta_access(&self, access_code: String) -> Result<bool, String> {
        let response = self
            .client
            .post(endpoint(
                DEFAULT_TEAM_RELAY_HUB_URL,
                "v1/beta-access/verify",
            ))
            .json(&json!({ "access_code": access_code }))
            .send()
            .await
            .map_err(network_error)?;
        if response.status().is_success() {
            return Ok(true);
        }
        let error = decode_error(response).await;
        if error.starts_with("beta_access_invalid:") {
            return Ok(false);
        }
        Err(error)
    }

    pub async fn register_account(
        &self,
        input: AccountEnrollmentInput,
    ) -> Result<TeamRelayAccountState, String> {
        let hub_url = normalize_hub_url(&input.hub_url)?;
        let response = self
            .client
            .post(endpoint(&hub_url, "v1/enrollments/account"))
            .json(&json!({
                "email": input.email,
                "display_name": input.display_name,
                "device_name": input.device_name,
            }))
            .send()
            .await
            .map_err(network_error)?;
        let enrollment: AccountEnrollmentResponse = decode(response).await?;
        let settings = TeamRelaySettings {
            hub_url,
            team_id: String::new(),
        };
        self.credentials
            .set(
                &credential_reference(&settings.hub_url),
                &enrollment.device_token,
            )
            .map_err(|error| format!("credential_store_failed: {}", error.user_message()))?;
        if let Err(error) = self.save_settings(&settings) {
            let _ = self
                .credentials
                .delete(&credential_reference(&settings.hub_url));
            return Err(error);
        }
        self.account_state().await
    }

    pub async fn account_state(&self) -> Result<TeamRelayAccountState, String> {
        let (settings, token) = self.configured()?;
        let me_response = self
            .client
            .get(endpoint(&settings.hub_url, "v1/me"))
            .bearer_auth(&token)
            .send()
            .await
            .map_err(network_error)?;
        let me: GetMeResponse = decode(me_response).await?;
        let teams_response = self
            .client
            .get(endpoint(&settings.hub_url, "v1/teams"))
            .bearer_auth(&token)
            .send()
            .await
            .map_err(network_error)?;
        let teams: ListTeamsResponse = decode(teams_response).await?;
        Ok(TeamRelayAccountState {
            principal: me.principal,
            teams: teams.teams,
        })
    }

    pub async fn claim(&self, input: ClaimEnrollmentInput) -> Result<TeamRelaySnapshot, String> {
        let hub_url = normalize_hub_url(&input.hub_url)?;
        let response = self
            .client
            .post(endpoint(&hub_url, "v1/enrollments/join"))
            .json(&json!({
                "join_password": input.join_password,
                "email": input.email,
                "display_name": input.display_name,
                "device_name": input.device_name,
            }))
            .send()
            .await
            .map_err(network_error)?;
        let claim: ClaimEnrollmentResponse = decode(response).await?;
        let settings = TeamRelaySettings {
            hub_url,
            team_id: claim.team.id.clone(),
        };
        self.credentials
            .set(
                &credential_reference(&settings.hub_url),
                &claim.device_token,
            )
            .map_err(|error| format!("credential_store_failed: {}", error.user_message()))?;
        if let Err(error) = self.save_settings(&settings) {
            let _ = self
                .credentials
                .delete(&credential_reference(&settings.hub_url));
            return Err(error);
        }
        self.bootstrap().await
    }

    pub async fn create_first_team(
        &self,
        input: CreateTeamEnrollmentInput,
    ) -> Result<TeamRelaySnapshot, String> {
        let hub_url = normalize_hub_url(&input.hub_url)?;
        let response = self
            .client
            .post(endpoint(&hub_url, "v1/enrollments/create-team"))
            .json(&json!({
                "team_name": input.team_name,
                "email": input.email,
                "display_name": input.display_name,
                "device_name": input.device_name,
            }))
            .send()
            .await
            .map_err(network_error)?;
        let enrollment: ClaimEnrollmentResponse = decode(response).await?;
        let settings = TeamRelaySettings {
            hub_url,
            team_id: enrollment.team.id,
        };
        self.credentials
            .set(
                &credential_reference(&settings.hub_url),
                &enrollment.device_token,
            )
            .map_err(|error| format!("credential_store_failed: {}", error.user_message()))?;
        if let Err(error) = self.save_settings(&settings) {
            let _ = self
                .credentials
                .delete(&credential_reference(&settings.hub_url));
            return Err(error);
        }
        self.bootstrap().await
    }

    pub async fn bootstrap(&self) -> Result<TeamRelaySnapshot, String> {
        let (settings, token) = self.configured()?;
        if settings.team_id.is_empty() {
            return Err("team_relay_team_required: create or join a team first".to_string());
        }
        self.bootstrap_for(&settings, &token).await
    }

    pub async fn update_profile(
        &self,
        input: UpdateProfileInput,
    ) -> Result<TeamRelaySnapshot, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .patch(endpoint(&settings.hub_url, "v1/me"))
            .bearer_auth(&token)
            .json(&input)
            .send()
            .await
            .map_err(network_error)?;
        let _: UpdateProfileResponse = decode(response).await?;
        self.bootstrap_for(&settings, &token).await
    }

    async fn bootstrap_for(
        &self,
        settings: &TeamRelaySettings,
        token: &str,
    ) -> Result<TeamRelaySnapshot, String> {
        let teams_response = self
            .client
            .get(endpoint(&settings.hub_url, "v1/teams"))
            .bearer_auth(token)
            .send()
            .await
            .map_err(network_error)?;
        let teams: ListTeamsResponse = decode(teams_response).await?;
        let bootstrap_requests = teams.teams.iter().map(|team| async {
            let response = self
                .client
                .get(endpoint(&settings.hub_url, "v1/bootstrap"))
                .bearer_auth(token)
                .query(&[("team_id", team.id.as_str())])
                .send()
                .await
                .map_err(network_error)?;
            decode::<BootstrapResponse>(response).await
        });
        let team_bootstraps = join_all(bootstrap_requests)
            .await
            .into_iter()
            .collect::<Result<Vec<_>, _>>()?;
        for bootstrap in &team_bootstraps {
            if !matches!(bootstrap.protocol_version.as_str(), "0.1" | "0.2") {
                return Err(format!(
                    "protocol_version_unsupported: {}",
                    bootstrap.protocol_version
                ));
            }
        }
        let bootstrap = team_bootstraps
            .iter()
            .find(|item| item.team.id == settings.team_id)
            .or_else(|| team_bootstraps.first())
            .cloned()
            .ok_or_else(|| "team_relay_team_required: create or join a team first".to_string())?;

        let attachment_ids: HashSet<String> = team_bootstraps
            .iter()
            .flat_map(|item| item.threads.iter())
            .flat_map(|thread| thread.actions.iter())
            .flat_map(|action| action.attachment_ids.iter().cloned())
            .collect();
        let attachment_requests = attachment_ids.into_iter().map(|attachment_id| async move {
            let response = self
                .client
                .get(endpoint(
                    &settings.hub_url,
                    &format!("v1/attachments/{attachment_id}"),
                ))
                .bearer_auth(&token)
                .send()
                .await
                .map_err(network_error)?;
            decode::<Attachment>(response).await
        });
        let mut attachments = HashMap::new();
        for attachment in join_all(attachment_requests).await {
            let attachment = attachment?;
            attachments.insert(attachment.id.clone(), attachment);
        }
        Ok(TeamRelaySnapshot {
            bootstrap,
            team_bootstraps,
            attachments,
            teams: teams.teams,
        })
    }

    pub async fn create_team(&self, name: &str) -> Result<TeamRelaySnapshot, String> {
        let (mut settings, token) = self.configured()?;
        let response = self
            .client
            .post(endpoint(&settings.hub_url, "v1/teams"))
            .bearer_auth(&token)
            .json(&json!({ "name": name }))
            .send()
            .await
            .map_err(network_error)?;
        let created: TeamMembershipResponse = decode(response).await?;
        settings.team_id = created.team.id;
        let snapshot = self.bootstrap_for(&settings, &token).await?;
        self.save_settings(&settings)?;
        Ok(snapshot)
    }

    pub async fn join_team(&self, join_code: &str) -> Result<TeamRelaySnapshot, String> {
        let (mut settings, token) = self.configured()?;
        let response = self
            .client
            .post(endpoint(&settings.hub_url, "v1/teams/join"))
            .bearer_auth(&token)
            .json(&json!({ "join_code": join_code }))
            .send()
            .await
            .map_err(network_error)?;
        let joined: TeamMembershipResponse = decode(response).await?;
        settings.team_id = joined.team.id;
        let snapshot = self.bootstrap_for(&settings, &token).await?;
        self.save_settings(&settings)?;
        Ok(snapshot)
    }

    pub async fn switch_team(&self, team_id: &str) -> Result<TeamRelaySnapshot, String> {
        let (mut settings, token) = self.configured()?;
        settings.team_id = team_id.to_string();
        let snapshot = self.bootstrap_for(&settings, &token).await?;
        self.save_settings(&settings)?;
        Ok(snapshot)
    }

    pub async fn create_invite(
        &self,
        expires_in_hours: u16,
    ) -> Result<CreateInviteResponse, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .post(endpoint(
                &settings.hub_url,
                &format!("v1/teams/{}/invites", settings.team_id),
            ))
            .bearer_auth(token)
            .json(&json!({ "expires_in_hours": expires_in_hours }))
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn get_join_password(&self) -> Result<TeamJoinCodeState, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .get(endpoint(
                &settings.hub_url,
                &format!("v1/teams/{}/join-password", settings.team_id),
            ))
            .bearer_auth(token)
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn rotate_join_password(&self) -> Result<TeamJoinCodeState, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .post(endpoint(
                &settings.hub_url,
                &format!("v1/teams/{}/join-password", settings.team_id),
            ))
            .bearer_auth(token)
            .json(&json!({}))
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn set_join_password_visibility(
        &self,
        visibility: &str,
    ) -> Result<TeamJoinCodeState, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .patch(endpoint(
                &settings.hub_url,
                &format!("v1/teams/{}/join-password", settings.team_id),
            ))
            .bearer_auth(token)
            .json(&json!({ "visibility": visibility }))
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn disable_join_password(&self) -> Result<TeamJoinCodeState, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .delete(endpoint(
                &settings.hub_url,
                &format!("v1/teams/{}/join-password", settings.team_id),
            ))
            .bearer_auth(token)
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn remove_member(&self, principal_id: &str) -> Result<(), String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .delete(endpoint(
                &settings.hub_url,
                &format!("v1/teams/{}/members/{principal_id}", settings.team_id),
            ))
            .bearer_auth(token)
            .send()
            .await
            .map_err(network_error)?;
        if !response.status().is_success() {
            return Err(decode_error(response).await);
        }
        Ok(())
    }

    pub async fn create_thread(
        &self,
        input: CreateThreadInput,
    ) -> Result<ThreadMutationResponse, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .post(endpoint(&settings.hub_url, "v1/threads"))
            .bearer_auth(token)
            .json(&json!({
                "team_id": input.team_id,
                "title": input.title,
                "assignee_id": input.assignee_id,
                "message": input.message,
                "attachment_ids": input.attachment_ids,
                "delivery": input.delivery,
                "idempotency_key": input.idempotency_key,
            }))
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn create_action(
        &self,
        thread_id: &str,
        input: CreateActionInput,
    ) -> Result<ThreadMutationResponse, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .post(endpoint(
                &settings.hub_url,
                &format!("v1/threads/{thread_id}/actions"),
            ))
            .bearer_auth(token)
            .json(&json!({
                "kind": input.kind,
                "recipient_id": input.recipient_id,
                "message": input.message,
                "attachment_ids": input.attachment_ids,
                "delivery": input.delivery,
                "expected_revision": input.expected_revision,
                "idempotency_key": input.idempotency_key,
            }))
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn set_thread_preference(
        &self,
        thread_id: &str,
        input: SetThreadPreferenceInput,
    ) -> Result<ThreadPreference, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .put(endpoint(
                &settings.hub_url,
                &format!("v1/threads/{thread_id}/preference"),
            ))
            .bearer_auth(token)
            .json(&input)
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn mark_thread_read(
        &self,
        thread_id: &str,
        input: MarkThreadReadInput,
    ) -> Result<ThreadReadState, String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .put(endpoint(
                &settings.hub_url,
                &format!("v1/threads/{thread_id}/read"),
            ))
            .bearer_auth(token)
            .json(&input)
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn upload_attachment(
        &self,
        app: AppHandle,
        path: &Path,
        upload_id: &str,
        team_id: &str,
    ) -> Result<Attachment, String> {
        let metadata =
            std::fs::metadata(path).map_err(|error| format!("attachment_unreadable: {error}"))?;
        if !metadata.is_file() {
            return Err("attachment_unreadable: selected path is not a file".to_string());
        }
        if metadata.len() > MAX_ATTACHMENT_BYTES {
            return Err("attachment_too_large: attachment exceeds 20 MiB".to_string());
        }
        let file_name = path
            .file_name()
            .and_then(|name| name.to_str())
            .ok_or_else(|| "attachment_invalid_name: filename is not valid UTF-8".to_string())?;
        let file = tokio::fs::File::open(path)
            .await
            .map_err(|error| format!("attachment_unreadable: {error}"))?;
        let total_bytes = metadata.len();
        let progress_upload_id = upload_id.to_string();
        let mut sent_bytes = 0_u64;
        let stream = ReaderStream::new(file).map(move |chunk| {
            if let Ok(bytes) = &chunk {
                sent_bytes = sent_bytes.saturating_add(bytes.len() as u64);
                let _ = app.emit(
                    "team_relay:upload-progress",
                    AttachmentUploadProgress {
                        upload_id: progress_upload_id.clone(),
                        sent_bytes,
                        total_bytes,
                    },
                );
            }
            chunk
        });
        let body = reqwest::Body::wrap_stream(stream);
        let (settings, token) = self.configured()?;
        let form = multipart::Form::new()
            .text("team_id", team_id.to_string())
            .part(
                "file",
                multipart::Part::stream_with_length(body, total_bytes)
                    .file_name(file_name.to_string()),
            );
        let response = self
            .client
            .post(endpoint(&settings.hub_url, "v1/attachments"))
            .bearer_auth(token)
            .header("Idempotency-Key", upload_id)
            .multipart(form)
            .send()
            .await
            .map_err(network_error)?;
        decode(response).await
    }

    pub async fn download_attachment(
        &self,
        attachment_id: &str,
        destination: &Path,
    ) -> Result<(), String> {
        let (settings, token) = self.configured()?;
        let response = self
            .client
            .get(endpoint(
                &settings.hub_url,
                &format!("v1/attachments/{attachment_id}/content"),
            ))
            .bearer_auth(token)
            .send()
            .await
            .map_err(network_error)?;
        if !response.status().is_success() {
            return Err(decode_error(response).await);
        }
        if response
            .content_length()
            .is_some_and(|size| size > MAX_ATTACHMENT_BYTES)
        {
            return Err("attachment_too_large: Hub returned more than 20 MiB".to_string());
        }
        if let Some(parent) = destination.parent() {
            std::fs::create_dir_all(parent)
                .map_err(|error| format!("attachment_save_failed: {error}"))?;
        }
        let parent = destination.parent().unwrap_or_else(|| Path::new("."));
        let file_name = destination
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or("attachment");
        let unique = format!(
            "{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos()
        );
        let temporary = parent.join(format!(".{file_name}.pinvou-download-{unique}"));
        let backup = parent.join(format!(".{file_name}.pinvou-backup-{unique}"));
        let mut file = tokio::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)
            .await
            .map_err(|error| format!("attachment_save_failed: {error}"))?;
        let mut received = 0_u64;
        let mut stream = response.bytes_stream();
        while let Some(chunk) = stream.next().await {
            let chunk = match chunk {
                Ok(chunk) => chunk,
                Err(error) => {
                    drop(file);
                    let _ = tokio::fs::remove_file(&temporary).await;
                    return Err(network_error(error));
                }
            };
            received += chunk.len() as u64;
            if received > MAX_ATTACHMENT_BYTES {
                drop(file);
                let _ = tokio::fs::remove_file(&temporary).await;
                return Err("attachment_too_large: Hub returned more than 20 MiB".to_string());
            }
            if let Err(error) = file.write_all(&chunk).await {
                drop(file);
                let _ = tokio::fs::remove_file(&temporary).await;
                return Err(format!("attachment_save_failed: {error}"));
            }
        }
        if let Err(error) = file.flush().await {
            drop(file);
            let _ = tokio::fs::remove_file(&temporary).await;
            return Err(format!("attachment_save_failed: {error}"));
        }
        if let Err(error) = file.sync_all().await {
            drop(file);
            let _ = tokio::fs::remove_file(&temporary).await;
            return Err(format!("attachment_save_failed: {error}"));
        }
        drop(file);
        let replace_result = if destination.exists() {
            crate::platform::filesystem::replace_file_atomically(&temporary, destination, &backup)
        } else {
            match std::fs::rename(&temporary, destination) {
                Ok(()) => Ok(()),
                Err(_) if destination.exists() => {
                    crate::platform::filesystem::replace_file_atomically(
                        &temporary,
                        destination,
                        &backup,
                    )
                }
                Err(error) => Err(error),
            }
        };
        if let Err(error) = replace_result {
            let _ = std::fs::remove_file(&temporary);
            let _ = std::fs::remove_file(&backup);
            return Err(format!("attachment_save_failed: {error}"));
        }
        if let Ok(directory) = std::fs::File::open(parent) {
            let _ = directory.sync_all();
        }
        Ok(())
    }

    pub fn task_workspace_status(
        &self,
        thread_id: &str,
        team_id: Option<&str>,
    ) -> Result<TeamRelayTaskWorkspaceStatus, String> {
        validate_local_identifier("thread_id", thread_id)?;
        let settings = self
            .load_settings()?
            .ok_or_else(|| "team_relay_not_configured: join a team first".to_string())?;
        let team_id = team_id
            .filter(|value| !value.is_empty())
            .unwrap_or(&settings.team_id);
        validate_local_identifier("team_id", team_id)?;
        let root = self.task_workspace_root(team_id, thread_id);
        let manifest = self.load_task_workspace_manifest(&root, team_id, thread_id)?;
        let materials_dir = root.join("materials");
        let materials = manifest
            .materials
            .into_iter()
            .filter(|material| {
                Path::new(&material.local_path).parent() == Some(materials_dir.as_path())
                    && std::fs::metadata(&material.local_path).is_ok_and(|metadata| {
                        metadata.is_file() && metadata.len() == material.byte_size
                    })
            })
            .collect();
        Ok(TeamRelayTaskWorkspaceStatus {
            thread_id: thread_id.to_string(),
            root_path: root.to_string_lossy().into_owned(),
            materials,
            agent_session: manifest.agent_session,
        })
    }

    pub async fn prepare_task_workspace(
        &self,
        thread_id: &str,
    ) -> Result<TeamRelayTaskWorkspaceStatus, String> {
        validate_local_identifier("thread_id", thread_id)?;
        let snapshot = self.bootstrap().await?;
        let detail = snapshot
            .team_bootstraps
            .iter()
            .flat_map(|bootstrap| bootstrap.threads.iter())
            .find(|detail| detail.thread.id == thread_id)
            .ok_or_else(|| "thread_not_found: the work item is not available".to_string())?;
        let team_id = detail.thread.team_id.as_str();
        let root = self.task_workspace_root(team_id, thread_id);
        let materials_dir = root.join("materials");
        std::fs::create_dir_all(&materials_dir)
            .map_err(|error| format!("team_relay_workspace_write_failed: {error}"))?;
        let mut manifest = self.load_task_workspace_manifest(&root, team_id, thread_id)?;
        let mut actions = detail.actions.iter().collect::<Vec<_>>();
        actions.sort_by_key(|action| action.sequence);
        let mut seen = HashSet::new();
        let mut material_entries = Vec::new();
        for action in actions {
            for attachment_id in &action.attachment_ids {
                if seen.insert(attachment_id.clone()) {
                    material_entries.push((action.sequence, attachment_id.clone()));
                }
            }
        }
        let mut base_name_counts = HashMap::new();
        for (sequence, attachment_id) in &material_entries {
            if let Some(attachment) = snapshot.attachments.get(attachment_id) {
                let name = material_file_name(*sequence, &attachment.original_name, None);
                *base_name_counts
                    .entry(name.to_lowercase())
                    .or_insert(0_usize) += 1;
            }
        }
        let mut prepared = Vec::new();
        let mut migrated_legacy_paths = Vec::new();
        for (sequence, attachment_id) in material_entries {
            let attachment = snapshot
                .attachments
                .get(&attachment_id)
                .filter(|attachment| attachment.available)
                .ok_or_else(|| {
                    format!(
                        "attachment_unavailable: shared material {attachment_id} is unavailable"
                    )
                })?;
            let base_name = material_file_name(sequence, &attachment.original_name, None);
            let needs_suffix = base_name_counts
                .get(&base_name.to_lowercase())
                .copied()
                .unwrap_or_default()
                > 1;
            let suffix = needs_suffix.then(|| path_fingerprint(&attachment.id));
            let destination = materials_dir.join(material_file_name(
                sequence,
                &attachment.original_name,
                suffix.as_deref(),
            ));
            let mut reusable = material_matches(&destination, attachment).await?;
            if !reusable {
                let legacy_path = legacy_material_path(&root, attachment);
                if material_matches(&legacy_path, attachment).await?
                    && copy_material(&legacy_path, &destination).is_ok()
                    && material_matches(&destination, attachment).await?
                {
                    reusable = true;
                    migrated_legacy_paths.push(legacy_path);
                }
            }
            if !reusable {
                self.download_attachment(&attachment.id, &destination)
                    .await?;
                let actual_hash = sha256_file(&destination).await?;
                if actual_hash != attachment.sha256 {
                    let _ = tokio::fs::remove_file(&destination).await;
                    return Err(format!(
                        "attachment_integrity_failed: {} did not match its shared checksum",
                        attachment.original_name
                    ));
                }
            }
            prepared.push(TeamRelayPreparedMaterial {
                attachment_id: attachment.id.clone(),
                original_name: attachment.original_name.clone(),
                local_path: destination.to_string_lossy().into_owned(),
                byte_size: attachment.byte_size,
                sha256: attachment.sha256.clone(),
                prepared_at: Utc::now().to_rfc3339(),
            });
        }
        manifest.materials = prepared.clone();
        self.save_task_workspace_manifest(&root, &manifest)?;
        cleanup_migrated_legacy_materials(&root, &migrated_legacy_paths);
        Ok(TeamRelayTaskWorkspaceStatus {
            thread_id: thread_id.to_string(),
            root_path: root.to_string_lossy().into_owned(),
            materials: prepared,
            agent_session: manifest.agent_session,
        })
    }

    pub fn reveal_prepared_material(
        &self,
        thread_id: &str,
        attachment_id: &str,
        team_id: Option<&str>,
    ) -> Result<(), String> {
        validate_local_identifier("thread_id", thread_id)?;
        validate_local_identifier("attachment_id", attachment_id)?;
        let settings = self
            .load_settings()?
            .ok_or_else(|| "team_relay_not_configured: join a team first".to_string())?;
        let team_id = team_id
            .filter(|value| !value.is_empty())
            .unwrap_or(&settings.team_id);
        validate_local_identifier("team_id", team_id)?;
        let root = self.task_workspace_root(team_id, thread_id);
        let manifest = self.load_task_workspace_manifest(&root, team_id, thread_id)?;
        let material = manifest
            .materials
            .iter()
            .find(|material| material.attachment_id == attachment_id)
            .ok_or_else(|| {
                "team_relay_material_not_prepared: prepare the material first".to_string()
            })?;
        let root = root
            .canonicalize()
            .map_err(|error| format!("team_relay_workspace_read_failed: {error}"))?;
        let path = PathBuf::from(&material.local_path)
            .canonicalize()
            .map_err(|error| format!("team_relay_material_not_prepared: {error}"))?;
        if !path.is_file() || !path.starts_with(&root) {
            return Err(
                "team_relay_workspace_invalid: material path escaped its workspace".to_string(),
            );
        }
        crate::platform::os::reveal_target(&path)
    }

    pub fn set_task_agent_session(
        &self,
        thread_id: &str,
        input: SetTeamRelayAgentSessionInput,
        team_id: Option<&str>,
    ) -> Result<TeamRelayTaskWorkspaceStatus, String> {
        validate_local_identifier("thread_id", thread_id)?;
        validate_local_identifier("session_id", &input.session_id)?;
        if let Some(agent_id) = input.agent_id.as_deref() {
            validate_local_identifier("agent_id", agent_id)?;
        }
        if input.material_ids.len() > 100 {
            return Err("team_relay_materials_invalid: too many linked materials".to_string());
        }
        for material_id in &input.material_ids {
            validate_local_identifier("material_id", material_id)?;
        }
        if !matches!(input.kind.as_str(), "chat" | "coding") {
            return Err("team_relay_agent_kind_invalid: expected chat or coding".to_string());
        }
        if input.kind == "coding"
            && input
                .workspace_path
                .as_deref()
                .is_some_and(|path| path.trim().is_empty() || !Path::new(path).is_absolute())
        {
            return Err(
                "team_relay_workspace_invalid: Coding workspace paths must be absolute".to_string(),
            );
        }
        let settings = self
            .load_settings()?
            .ok_or_else(|| "team_relay_not_configured: join a team first".to_string())?;
        let team_id = team_id
            .filter(|value| !value.is_empty())
            .unwrap_or(&settings.team_id);
        validate_local_identifier("team_id", team_id)?;
        let root = self.task_workspace_root(team_id, thread_id);
        let mut manifest = self.load_task_workspace_manifest(&root, team_id, thread_id)?;
        manifest.agent_session = Some(TeamRelayAgentSessionLink {
            kind: input.kind,
            session_id: input.session_id,
            agent_id: input.agent_id,
            workspace_path: input.workspace_path,
            material_ids: input.material_ids,
            last_synced_sequence: input.last_synced_sequence,
            linked_at: Utc::now().to_rfc3339(),
        });
        self.save_task_workspace_manifest(&root, &manifest)?;
        self.task_workspace_status(thread_id, Some(team_id))
    }

    pub fn start_events(&self, app: AppHandle, team_ids: &[String]) -> Result<(), String> {
        let (settings, token) = self.configured()?;
        let mut subscribed_team_ids = team_ids.to_vec();
        subscribed_team_ids.sort();
        let scope_key = format!("{}:{}", settings.hub_url, subscribed_team_ids.join(","));
        let mut task = self
            .event_task
            .lock()
            .map_err(|_| "team_relay_runtime_unavailable".to_string())?;
        if task
            .as_ref()
            .is_some_and(|active| active.scope_key == scope_key)
        {
            return Ok(());
        }
        if let Some(active) = task.take() {
            active.handle.abort();
        }
        let client = self.event_client.clone();
        let handle = tauri::async_runtime::spawn(async move {
            loop {
                let request = client
                    .get(endpoint(&settings.hub_url, "v1/events"))
                    .bearer_auth(&token)
                    .send()
                    .await;
                if let Ok(response) = request {
                    if response.status().is_success() {
                        // Events are transient hints, so every successful (re)connect must
                        // first trigger an authoritative bootstrap to cover an offline gap.
                        let _ = app.emit(
                            "team_relay:changed",
                            json!({ "event": "resync", "team_id": settings.team_id }),
                        );
                        let mut stream = response.bytes_stream();
                        let mut buffer = String::new();
                        loop {
                            // The Hub writes a heartbeat every 20 seconds. If no frame arrives
                            // for 45 seconds, treat the connection as stale and reconnect.
                            let chunk =
                                match tokio::time::timeout(Duration::from_secs(45), stream.next())
                                    .await
                                {
                                    Ok(Some(chunk)) => chunk,
                                    _ => break,
                                };
                            let Ok(chunk) = chunk else { break };
                            buffer.push_str(&String::from_utf8_lossy(&chunk));
                            while let Some(index) = buffer.find("\n\n") {
                                let frame = buffer[..index].to_string();
                                buffer.drain(..index + 2);
                                let event =
                                    frame.lines().find_map(|line| line.strip_prefix("event: "));
                                if let Some(name) = event.filter(|name| *name != "ready") {
                                    if name == "thread.attention" {
                                        crate::platform::notifications::notify_team_relay_changed(
                                            &app,
                                        );
                                    }
                                    let _ =
                                        app.emit("team_relay:changed", json!({ "event": name }));
                                }
                            }
                        }
                    }
                }
                tokio::time::sleep(Duration::from_secs(3)).await;
            }
        });
        *task = Some(TeamRelayEventTask { scope_key, handle });
        Ok(())
    }

    pub fn disconnect(&self) -> Result<(), String> {
        if let Some(task) = self
            .event_task
            .lock()
            .map_err(|_| "team_relay_runtime_unavailable".to_string())?
            .take()
        {
            task.handle.abort();
        }
        if let Some(settings) = self.load_settings()? {
            self.credentials
                .delete(&credential_reference(&settings.hub_url))
                .map_err(|error| format!("credential_store_failed: {}", error.user_message()))?;
        }
        match std::fs::remove_file(&self.settings_path) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(format!("team_relay_settings_delete_failed: {error}")),
        }
    }

    fn configured(&self) -> Result<(TeamRelaySettings, String), String> {
        let settings = self
            .load_settings()?
            .ok_or_else(|| "team_relay_not_configured: join a team first".to_string())?;
        let token = self
            .token(&settings)?
            .ok_or_else(|| "authentication_required: device credential is missing".to_string())?;
        Ok((settings, token))
    }

    fn token(&self, settings: &TeamRelaySettings) -> Result<Option<String>, String> {
        self.credentials
            .get(&credential_reference(&settings.hub_url))
            .map_err(|error| format!("credential_store_failed: {}", error.user_message()))
    }

    fn load_settings(&self) -> Result<Option<TeamRelaySettings>, String> {
        let text = match std::fs::read_to_string(&self.settings_path) {
            Ok(text) => text,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(error) => return Err(format!("team_relay_settings_read_failed: {error}")),
        };
        serde_json::from_str(&text)
            .map(Some)
            .map_err(|error| format!("team_relay_settings_invalid: {error}"))
    }

    fn save_settings(&self, settings: &TeamRelaySettings) -> Result<(), String> {
        let parent = self
            .settings_path
            .parent()
            .expect("team relay settings parent");
        std::fs::create_dir_all(parent)
            .map_err(|error| format!("team_relay_settings_write_failed: {error}"))?;
        let payload = serde_json::to_vec_pretty(settings)
            .map_err(|error| format!("team_relay_settings_write_failed: {error}"))?;
        deepseek_tui::utils::write_atomic(&self.settings_path, &payload)
            .map_err(|error| format!("team_relay_settings_write_failed: {error}"))
    }

    fn task_workspace_root(&self, team_id: &str, thread_id: &str) -> PathBuf {
        self.settings_path
            .parent()
            .expect("team relay settings parent")
            .join("workspaces")
            .join(path_fingerprint(team_id))
            .join(path_fingerprint(thread_id))
    }

    fn load_task_workspace_manifest(
        &self,
        root: &Path,
        team_id: &str,
        thread_id: &str,
    ) -> Result<TeamRelayTaskWorkspaceManifest, String> {
        let path = root.join("workspace.json");
        let text = match std::fs::read_to_string(&path) {
            Ok(text) => text,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(TeamRelayTaskWorkspaceManifest {
                    schema_version: 1,
                    team_id: team_id.to_string(),
                    thread_id: thread_id.to_string(),
                    materials: Vec::new(),
                    agent_session: None,
                });
            }
            Err(error) => {
                return Err(format!("team_relay_workspace_read_failed: {error}"));
            }
        };
        let manifest: TeamRelayTaskWorkspaceManifest = serde_json::from_str(&text)
            .map_err(|error| format!("team_relay_workspace_invalid: {error}"))?;
        if manifest.schema_version != 1
            || manifest.team_id != team_id
            || manifest.thread_id != thread_id
        {
            return Err("team_relay_workspace_invalid: manifest identity mismatch".to_string());
        }
        Ok(manifest)
    }

    fn save_task_workspace_manifest(
        &self,
        root: &Path,
        manifest: &TeamRelayTaskWorkspaceManifest,
    ) -> Result<(), String> {
        std::fs::create_dir_all(root)
            .map_err(|error| format!("team_relay_workspace_write_failed: {error}"))?;
        let payload = serde_json::to_vec_pretty(manifest)
            .map_err(|error| format!("team_relay_workspace_write_failed: {error}"))?;
        deepseek_tui::utils::write_atomic(&root.join("workspace.json"), &payload)
            .map_err(|error| format!("team_relay_workspace_write_failed: {error}"))
    }
}

fn validate_local_identifier(label: &str, value: &str) -> Result<(), String> {
    if value.is_empty()
        || value.len() > 160
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err(format!("team_relay_identifier_invalid: invalid {label}"));
    }
    Ok(())
}

fn path_fingerprint(value: &str) -> String {
    Sha256::digest(value.as_bytes())
        .iter()
        .take(12)
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

fn safe_material_name(value: &str) -> String {
    let leaf = value
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or("material")
        .trim();
    let sanitized = leaf
        .chars()
        .map(|character| {
            if character.is_alphanumeric() || matches!(character, '.' | '-' | '_' | ' ') {
                character
            } else {
                '_'
            }
        })
        .collect::<String>();
    let sanitized = sanitized.trim_matches(['.', ' ']);
    if sanitized.is_empty() {
        "material".to_string()
    } else {
        sanitized.chars().take(180).collect()
    }
}

fn material_file_name(sequence: u64, original_name: &str, suffix: Option<&str>) -> String {
    const MAX_FILE_NAME_BYTES: usize = 180;

    let leaf = original_name
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or("material")
        .trim();
    let path = Path::new(leaf);
    let extension = path
        .extension()
        .and_then(|value| value.to_str())
        .map(|value| {
            value
                .chars()
                .map(|character| {
                    if character.is_alphanumeric() || matches!(character, '-' | '_') {
                        character
                    } else {
                        '_'
                    }
                })
                .take(20)
                .collect::<String>()
        })
        .filter(|value| !value.is_empty());
    let stem = safe_material_name(
        path.file_stem()
            .and_then(|value| value.to_str())
            .filter(|value| !value.is_empty())
            .unwrap_or("material"),
    );
    let prefix = format!("P{sequence:02}-");
    let suffix = suffix.map(|value| format!("--{value}")).unwrap_or_default();
    let extension = extension
        .map(|value| format!(".{value}"))
        .unwrap_or_default();
    let reserved = prefix.len() + suffix.len() + extension.len();
    let stem = truncate_utf8(&stem, MAX_FILE_NAME_BYTES.saturating_sub(reserved).max(1));
    format!("{prefix}{stem}{suffix}{extension}")
}

fn truncate_utf8(value: &str, max_bytes: usize) -> &str {
    if value.len() <= max_bytes {
        return value;
    }
    let mut end = max_bytes.min(value.len());
    while end > 0 && !value.is_char_boundary(end) {
        end -= 1;
    }
    &value[..end]
}

fn legacy_material_path(root: &Path, attachment: &Attachment) -> PathBuf {
    root.join("inputs")
        .join(path_fingerprint(&attachment.id))
        .join(safe_material_name(&attachment.original_name))
}

async fn material_matches(path: &Path, attachment: &Attachment) -> Result<bool, String> {
    match tokio::fs::metadata(path).await {
        Ok(metadata) if metadata.is_file() && metadata.len() == attachment.byte_size => {
            Ok(sha256_file(path).await? == attachment.sha256)
        }
        Ok(_) => Ok(false),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(format!("attachment_read_failed: {error}")),
    }
}

fn copy_material(source: &Path, destination: &Path) -> Result<(), String> {
    let parent = destination.parent().ok_or_else(|| {
        format!(
            "team_relay_workspace_write_failed: no parent for {}",
            destination.display()
        )
    })?;
    std::fs::create_dir_all(parent)
        .map_err(|error| format!("team_relay_workspace_write_failed: {error}"))?;
    std::fs::copy(source, destination)
        .map(|_| ())
        .map_err(|error| format!("team_relay_workspace_write_failed: {error}"))
}

fn cleanup_migrated_legacy_materials(root: &Path, paths: &[PathBuf]) {
    let inputs = root.join("inputs");
    for path in paths {
        if !path.starts_with(&inputs) {
            continue;
        }
        if std::fs::remove_file(path).is_ok() {
            if let Some(parent) = path.parent() {
                let _ = std::fs::remove_dir(parent);
            }
        }
    }
    let _ = std::fs::remove_dir(inputs);
}

async fn sha256_file(path: &Path) -> Result<String, String> {
    let bytes = tokio::fs::read(path)
        .await
        .map_err(|error| format!("attachment_read_failed: {error}"))?;
    Ok(Sha256::digest(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect())
}

fn endpoint(hub_url: &str, path: &str) -> String {
    format!(
        "{}/{}",
        hub_url.trim_end_matches('/'),
        path.trim_start_matches('/')
    )
}

fn credential_reference(hub_url: &str) -> CredentialReference {
    let fingerprint = Sha256::digest(hub_url.as_bytes())
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    CredentialReference::for_team_relay_device(&fingerprint)
}

fn normalize_hub_url(value: &str) -> Result<String, String> {
    let mut url = Url::parse(value.trim())
        .map_err(|_| "hub_url_invalid: enter a valid Team Relay Hub URL".to_string())?;
    if !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err("hub_url_invalid: credentials, query and fragment are not allowed".to_string());
    }
    let host = url.host_str().unwrap_or_default();
    let local_http = matches!(host, "localhost" | "127.0.0.1" | "::1");
    if url.scheme() != "https" && !(url.scheme() == "http" && local_http) {
        return Err(
            "hub_url_insecure: HTTPS is required except for this device's localhost".to_string(),
        );
    }
    let normalized_path = url.path().trim_end_matches('/').to_string();
    url.set_path(&normalized_path);
    Ok(url.to_string().trim_end_matches('/').to_string())
}

async fn decode<T: DeserializeOwned>(response: Response) -> Result<T, String> {
    if !response.status().is_success() {
        return Err(decode_error(response).await);
    }
    response
        .json::<T>()
        .await
        .map_err(|error| format!("hub_response_invalid: {error}"))
}

async fn decode_error(response: Response) -> String {
    let status = response.status();
    match response.json::<HubErrorEnvelope>().await {
        Ok(envelope) => format!(
            "{}: {} (request {})",
            envelope.error.code, envelope.error.message, envelope.error.request_id
        ),
        Err(_) => format!("hub_http_error: HTTP {}", status.as_u16()),
    }
}

fn network_error(error: reqwest::Error) -> String {
    if error.is_timeout() {
        "hub_timeout: Team Relay Hub did not respond in time".to_string()
    } else if error.status() == Some(StatusCode::UNAUTHORIZED) {
        "authentication_required: device credential is invalid".to_string()
    } else {
        format!("hub_unreachable: {error}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalizes_https_hub_with_base_path() {
        assert_eq!(
            normalize_hub_url(" https://example.com/pinvou3/team-relay/ ").unwrap(),
            "https://example.com/pinvou3/team-relay"
        );
    }

    #[test]
    fn only_allows_plain_http_on_same_device() {
        assert!(normalize_hub_url("http://127.0.0.1:8791").is_ok());
        assert!(normalize_hub_url("http://192.168.1.8:8791").is_err());
        assert!(normalize_hub_url("https://example.com?token=nope").is_err());
    }

    #[test]
    fn credential_account_is_stable_and_does_not_contain_url() {
        let reference = credential_reference("https://example.com/team-relay");
        assert!(reference.account.starts_with("hub:"));
        assert!(!reference.account.contains("example.com"));
    }

    #[test]
    fn task_workspace_paths_do_not_embed_remote_identifiers() {
        let fingerprint = path_fingerprint("thread/customer-visible-identifier");
        assert_eq!(fingerprint.len(), 24);
        assert!(fingerprint.bytes().all(|byte| byte.is_ascii_hexdigit()));
        assert!(!fingerprint.contains("customer"));
    }

    #[test]
    fn shared_material_names_cannot_escape_the_task_workspace() {
        assert_eq!(safe_material_name("../../brief.md"), "brief.md");
        assert_eq!(safe_material_name(r"..\windows\brief?.md"), "brief_.md");
        assert_eq!(safe_material_name(".."), "material");
    }

    #[test]
    fn prepared_material_names_are_readable_ordered_and_collision_safe() {
        assert_eq!(material_file_name(1, "brief.md", None), "P01-brief.md");
        assert_eq!(material_file_name(12, "../brief.md", None), "P12-brief.md");
        assert_eq!(
            material_file_name(1, "brief.md", Some("5b5c9c6393ec")),
            "P01-brief--5b5c9c6393ec.md"
        );
    }

    #[test]
    fn prepared_material_names_stay_within_a_portable_byte_limit() {
        let name = material_file_name(2, &format!("{}.pdf", "材料".repeat(100)), None);
        assert!(name.len() <= 180);
        assert!(name.starts_with("P02-"));
        assert!(name.ends_with(".pdf"));
    }

    #[tokio::test]
    async fn verified_legacy_material_is_migrated_then_cleaned_up() {
        let root = std::env::temp_dir().join(format!(
            "pinvou-team-relay-migration-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let attachment = Attachment {
            id: "attachment-1".to_string(),
            team_id: "team-1".to_string(),
            uploaded_by: "member-1".to_string(),
            original_name: "brief.md".to_string(),
            content_type: "text/markdown".to_string(),
            byte_size: 5,
            sha256: Sha256::digest(b"brief")
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect(),
            bound: true,
            available: true,
            created_at: "2026-08-10T00:00:00Z".to_string(),
        };
        let legacy = legacy_material_path(&root, &attachment);
        std::fs::create_dir_all(legacy.parent().unwrap()).unwrap();
        std::fs::write(&legacy, b"brief").unwrap();
        let destination = root.join("materials/P01-brief.md");

        assert!(material_matches(&legacy, &attachment).await.unwrap());
        copy_material(&legacy, &destination).unwrap();
        assert!(material_matches(&destination, &attachment).await.unwrap());
        cleanup_migrated_legacy_materials(&root, std::slice::from_ref(&legacy));

        assert!(destination.is_file());
        assert!(!legacy.exists());
        assert!(!root.join("inputs").exists());
        std::fs::remove_dir_all(&root).unwrap();
    }

    #[test]
    fn local_link_identifiers_are_bounded_and_path_free() {
        assert!(validate_local_identifier("thread_id", "thread_01-abc").is_ok());
        assert!(validate_local_identifier("thread_id", "../thread").is_err());
        assert!(validate_local_identifier("session_id", "session/id").is_err());
    }

    #[test]
    fn older_agent_links_gain_safe_context_tracking_defaults() {
        let link: TeamRelayAgentSessionLink = serde_json::from_value(json!({
            "kind": "coding",
            "session_id": "session-1",
            "workspace_path": "/workspace/project",
            "linked_at": "2026-08-07T00:00:00Z"
        }))
        .unwrap();
        assert_eq!(link.agent_id, None);
        assert!(link.material_ids.is_empty());
        assert_eq!(link.last_synced_sequence, 0);
    }
}
