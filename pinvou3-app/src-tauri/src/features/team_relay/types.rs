use serde::{Deserialize, Serialize};
use std::collections::HashMap;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct TeamRelaySettings {
    pub hub_url: String,
    pub team_id: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Principal {
    pub id: String,
    #[serde(default)]
    pub email: Option<String>,
    pub display_name: String,
    pub created_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Device {
    pub id: String,
    pub principal_id: String,
    pub name: String,
    pub last_seen_at: Option<String>,
    pub created_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Team {
    pub id: String,
    pub name: String,
    pub created_by: String,
    pub created_at: String,
    #[serde(default)]
    pub join_password_configured: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TeamSummary {
    pub id: String,
    pub name: String,
    pub created_by: String,
    pub created_at: String,
    #[serde(default)]
    pub join_password_configured: bool,
    pub role: String,
    pub joined_at: String,
    #[serde(default)]
    pub member_count: u64,
    #[serde(default)]
    pub open_work_count: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ListTeamsResponse {
    #[serde(default)]
    pub teams: Vec<TeamSummary>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Membership {
    pub team_id: String,
    pub principal_id: String,
    pub role: String,
    pub joined_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TeamMember {
    pub id: String,
    pub display_name: String,
    #[serde(default)]
    pub email_hint: Option<String>,
    pub created_at: String,
    pub role: String,
    pub last_seen_at: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WorkThread {
    pub id: String,
    pub team_id: String,
    pub title: String,
    pub initiator_id: String,
    pub owner_id: String,
    pub current_actor_id: Option<String>,
    pub return_to_id: Option<String>,
    pub final_recipient_id: String,
    pub phase: String,
    pub revision: u64,
    pub created_at: String,
    pub updated_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct GitReference {
    pub repository_url: String,
    #[serde(default)]
    pub commit_sha: String,
    #[serde(default)]
    pub branch: String,
    #[serde(default)]
    pub pull_request_url: String,
    #[serde(default)]
    pub paths: Vec<String>,
    #[serde(default)]
    pub note: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeliveryPackage {
    pub schema_version: u8,
    pub artifact_role: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub subtype: String,
    #[serde(default)]
    pub expected_next: String,
    #[serde(default)]
    pub open_questions: String,
    #[serde(default)]
    pub git_references: Vec<GitReference>,
}

impl Default for DeliveryPackage {
    fn default() -> Self {
        Self {
            schema_version: 1,
            artifact_role: "intermediate".to_string(),
            subtype: String::new(),
            expected_next: String::new(),
            open_questions: String::new(),
            git_references: Vec::new(),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RelayAction {
    pub id: String,
    pub thread_id: String,
    pub sequence: u64,
    pub kind: String,
    pub actor_id: String,
    pub recipient_id: Option<String>,
    pub return_to_id: Option<String>,
    pub message: String,
    pub base_revision: u64,
    pub idempotency_key: String,
    pub attachment_ids: Vec<String>,
    #[serde(default)]
    pub delivery: DeliveryPackage,
    pub created_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ThreadDetail {
    pub thread: WorkThread,
    #[serde(default)]
    pub preference: ThreadPreference,
    pub participants: Vec<String>,
    pub actions: Vec<RelayAction>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct ThreadPreference {
    pub thread_id: String,
    pub pinned: bool,
    pub hidden: bool,
    pub pinned_at: Option<String>,
    pub hidden_at: Option<String>,
    pub updated_at: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BootstrapResponse {
    pub protocol_version: String,
    pub generated_at: String,
    pub team: Team,
    pub me: Principal,
    pub members: Vec<TeamMember>,
    pub threads: Vec<ThreadDetail>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Attachment {
    pub id: String,
    pub team_id: String,
    pub uploaded_by: String,
    pub original_name: String,
    pub content_type: String,
    pub byte_size: u64,
    pub sha256: String,
    pub bound: bool,
    pub available: bool,
    pub created_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AttachmentUploadProgress {
    pub upload_id: String,
    pub sent_bytes: u64,
    pub total_bytes: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TeamRelaySnapshot {
    pub bootstrap: BootstrapResponse,
    #[serde(default)]
    pub team_bootstraps: Vec<BootstrapResponse>,
    pub attachments: HashMap<String, Attachment>,
    #[serde(default)]
    pub teams: Vec<TeamSummary>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ClaimEnrollmentResponse {
    pub device_token: String,
    pub principal: Principal,
    pub device: Device,
    pub team: Team,
    pub membership: Membership,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AccountEnrollmentResponse {
    pub device_token: String,
    pub principal: Principal,
    pub device: Device,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TeamRelayAccountState {
    pub principal: Principal,
    #[serde(default)]
    pub teams: Vec<TeamSummary>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TeamMembershipResponse {
    pub team: Team,
    pub membership: Membership,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CreateInviteResponse {
    pub invite_id: String,
    pub invite_code: String,
    pub role: String,
    pub expires_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TeamJoinCodeState {
    pub configured: bool,
    pub visible: bool,
    pub recoverable: bool,
    pub visibility: String,
    pub join_password: Option<String>,
    pub updated_at: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ThreadMutationResponse {
    pub thread: WorkThread,
    pub action: RelayAction,
    pub replayed: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TeamRelayStatus {
    pub configured: bool,
    pub hub_url: Option<String>,
    pub team_id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct TeamRelayPreparedMaterial {
    pub attachment_id: String,
    pub original_name: String,
    pub local_path: String,
    pub byte_size: u64,
    pub sha256: String,
    pub prepared_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct TeamRelayAgentSessionLink {
    pub kind: String,
    pub session_id: String,
    #[serde(default)]
    pub agent_id: Option<String>,
    pub workspace_path: Option<String>,
    #[serde(default)]
    pub material_ids: Vec<String>,
    #[serde(default)]
    pub last_synced_sequence: u64,
    pub linked_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct TeamRelayTaskWorkspaceStatus {
    pub thread_id: String,
    pub root_path: String,
    #[serde(default)]
    pub materials: Vec<TeamRelayPreparedMaterial>,
    pub agent_session: Option<TeamRelayAgentSessionLink>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SetTeamRelayAgentSessionInput {
    pub kind: String,
    pub session_id: String,
    #[serde(default)]
    pub agent_id: Option<String>,
    pub workspace_path: Option<String>,
    #[serde(default)]
    pub material_ids: Vec<String>,
    #[serde(default)]
    pub last_synced_sequence: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub(crate) struct TeamRelayTaskWorkspaceManifest {
    pub schema_version: u8,
    pub team_id: String,
    pub thread_id: String,
    #[serde(default)]
    pub materials: Vec<TeamRelayPreparedMaterial>,
    pub agent_session: Option<TeamRelayAgentSessionLink>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ClaimEnrollmentInput {
    pub hub_url: String,
    pub join_password: String,
    pub email: String,
    pub display_name: String,
    pub device_name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AccountEnrollmentInput {
    pub hub_url: String,
    pub email: String,
    pub display_name: String,
    pub device_name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CreateTeamEnrollmentInput {
    pub hub_url: String,
    pub team_name: String,
    pub email: String,
    pub display_name: String,
    pub device_name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UpdateProfileInput {
    pub display_name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub email: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UpdateProfileResponse {
    pub principal: Principal,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CreateThreadInput {
    pub team_id: String,
    pub title: String,
    pub assignee_id: String,
    pub message: String,
    #[serde(default)]
    pub attachment_ids: Vec<String>,
    #[serde(default)]
    pub delivery: DeliveryPackage,
    pub idempotency_key: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CreateActionInput {
    pub kind: String,
    pub recipient_id: Option<String>,
    pub message: String,
    #[serde(default)]
    pub attachment_ids: Vec<String>,
    #[serde(default)]
    pub delivery: DeliveryPackage,
    pub expected_revision: u64,
    pub idempotency_key: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SetThreadPreferenceInput {
    pub pinned: bool,
    pub hidden: bool,
}
