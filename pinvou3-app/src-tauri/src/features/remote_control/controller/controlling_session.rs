//! A 端远程会话与任务意图（E2E-03）。
//!
//! 本模块只拥有「会话意图」这一份内存状态：它绑定当前 lease
//! （connection_id / controller_lease_id / lease_epoch / relay_epoch /
//! remote_control_generation）与协商能力，并按冻结 schema 构造 A 端发往 B 的
//! payload（能力快照请求、任务提交、任务取消）与输入摘要。前端只提交 UI 级
//! 字段（标题、正文、模式），不再拼装任何协议字段。
//!
//! 可选模式来自 B 的认证能力快照；模型来源支持 B 本机模型（`controlled_device`），
//! 以及租约已协商 `temporary_model_package_v1` 时、仅用于普通对话的 A 端临时模型。
//! 工作区、工具和 Skill 只投影受控 opaque ID 与显示名。
//! 任何上下文不匹配（无连接、lease 变更、未取到能力快照）都失败关闭。

use std::collections::BTreeMap;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use parking_lot::Mutex;
use serde::Serialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use uuid::Uuid;

use super::protocol::{
    LeaseContext, MAX_TASK_INPUT_TEXT_BYTES, RuntimeValidationContext, SenderRole,
    is_valid_identifier, validate_controller_runtime_payload_json,
};

/// 新会话意图的默认模式；每个任务仍必须按最新能力快照显式选择模式。
const SESSION_MODE_CHAT: &str = "chat";
/// 使用被控设备（B）本机模型。
const MODEL_SOURCE_CONTROLLED: &str = "controlled_device";
/// 使用主控设备（A）为当前任务加密传输的临时模型。
const MODEL_SOURCE_TEMPORARY: &str = "controller_temporary";
/// 一个连接上同时存在的会话意图上限（内存态，防无界增长）。
const MAX_SESSION_INTENTS: usize = 4;
const MAX_TITLE_CHARS: usize = 120;
/// 一个会话内跟踪的任务意图上限（超出后拒绝新提交，绝不静默丢弃）。
const MAX_TRACKED_TASKS: usize = 64;
const MAX_WORKSPACES: usize = 16;
const MAX_MODES: usize = 2;
const MAX_PENDING_CAPABILITY_REQUESTS: usize = 4;
const MAX_A_CAPABILITY_TTL_MS: u64 = 30_000;
/// 输入摘要的域分离前缀：同参必须同摘要、异参必须不同摘要。
const TASK_DIGEST_DOMAIN: &[u8] = b"pinvou-controller-task-input-v1";

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) struct SessionError {
    pub(crate) code: &'static str,
}

impl SessionError {
    pub(crate) const fn new(code: &'static str) -> Self {
        Self { code }
    }
}

impl std::fmt::Display for SessionError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.code)
    }
}

impl std::error::Error for SessionError {}

type Result<T> = std::result::Result<T, SessionError>;

/// 前端可见的会话投影：只含 opaque 标识、模式与计数，不含任何凭证。
#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControllingSessionProjection {
    pub(crate) session_ref: String,
    /// 该远程会话在 wire 上使用的 session id（A 生成；B 的派发层据此绑定权威
    /// 会话）。UI 持有的「远程 sessionId」即此值，`session_ref` 只用于本地
    /// 历史/多会话索引。
    pub(crate) session_id: String,
    pub(crate) title: String,
    pub(crate) mode: String,
    pub(crate) model_source: String,
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: String,
    pub(crate) relay_epoch: String,
    pub(crate) remote_control_generation: String,
    pub(crate) capability_revision: Option<u64>,
    pub(crate) capability_valid_for_ms: Option<u64>,
    pub(crate) capability_error: Option<String>,
    pub(crate) capability_request_id: Option<String>,
    /// Whether this lease negotiated the encrypted temporary-model package
    /// family. Expose only the UI decision, never the full protocol transcript.
    pub(crate) temporary_model_available: bool,
    pub(crate) modes: Vec<String>,
    pub(crate) workspace_count: usize,
    pub(crate) task_count: usize,
}

/// Only schema-approved, non-sensitive descriptor fields from an authenticated
/// B snapshot. This is never the raw wire payload or B's local capability owner.
#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControllerCapabilityProjection {
    pub(crate) remote_session_ref: String,
    pub(crate) connection_id: String,
    pub(crate) controller_lease_id: String,
    pub(crate) lease_epoch: String,
    pub(crate) relay_epoch: String,
    pub(crate) remote_control_generation: String,
    pub(crate) capability_revision: u64,
    pub(crate) expires_at_ms: u64,
    pub(crate) modes: Vec<String>,
    pub(crate) tools: Vec<ControllerToolDescriptor>,
    pub(crate) skills: Vec<ControllerSkillDescriptor>,
    pub(crate) workspaces: Vec<ControllerWorkspaceDescriptor>,
}

impl ControllerCapabilityProjection {
    pub(crate) fn revision(&self) -> u64 {
        self.capability_revision
    }
}

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControllerToolDescriptor {
    pub(crate) tool_id: String,
    pub(crate) display_name: String,
}

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControllerSkillDescriptor {
    pub(crate) skill_id: String,
    pub(crate) display_name: String,
    pub(crate) scopes: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControllerWorkspaceDescriptor {
    pub(crate) workspace_id: String,
    pub(crate) display_name: String,
    pub(crate) code_allowed: bool,
}

/// 一次意图的投影：冻结 payload + 前端需要的标识（不含任何密钥材料）。
#[derive(Debug, Clone, Serialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ControllingIntentProjection {
    pub(crate) request_id: String,
    pub(crate) session_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) task_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) client_request_id: Option<String>,
    pub(crate) payload: Value,
}

#[derive(Clone)]
struct SessionIntent {
    session_ref: String,
    session_id: String,
    title: String,
    mode: String,
    model_source: String,
    lease: LeaseContext,
    negotiated_capabilities: Vec<String>,
    capability_revision: Option<u64>,
    capability_valid_for_ms: Option<u64>,
    capability_projection: Option<ControllerCapabilityProjection>,
    capability_deadline: Option<Instant>,
    capability_error: Option<String>,
    last_capability_request_id: Option<String>,
    pending_capability_requests: Vec<String>,
    modes: Vec<String>,
    workspace_ids: Vec<String>,
    /// task_id -> client_request_id（同参重放可复用同一 client_request_id）。
    tasks: BTreeMap<String, String>,
    request_counter: u64,
}

impl SessionIntent {
    fn runtime_context(&self) -> RuntimeValidationContext {
        RuntimeValidationContext {
            lease: Some(self.lease.clone()),
            negotiated_capabilities: self.negotiated_capabilities.clone(),
        }
    }

    fn projection(&self) -> ControllingSessionProjection {
        ControllingSessionProjection {
            session_ref: self.session_ref.clone(),
            session_id: self.session_id.clone(),
            title: self.title.clone(),
            mode: self.mode.clone(),
            model_source: self.model_source.clone(),
            connection_id: self.lease.connection_id.clone(),
            controller_lease_id: self.lease.controller_lease_id.clone(),
            lease_epoch: self.lease.lease_epoch.clone(),
            relay_epoch: self.lease.relay_epoch.clone(),
            remote_control_generation: self.lease.remote_control_generation.clone(),
            capability_revision: self.capability_revision,
            capability_valid_for_ms: self.capability_valid_for_ms,
            capability_error: self.capability_error.clone(),
            capability_request_id: self.last_capability_request_id.clone(),
            temporary_model_available: self
                .negotiated_capabilities
                .iter()
                .any(|capability| capability == "temporary_model_package_v1"),
            modes: self.modes.clone(),
            workspace_count: self.workspace_ids.len(),
            task_count: self.tasks.len(),
        }
    }

    fn next_request_id(&mut self) -> String {
        self.request_counter += 1;
        format!("request-{}-{}", self.session_id, self.request_counter)
    }
}

#[derive(Default)]
pub(crate) struct ControllingSessionManager {
    intents: Mutex<Vec<SessionIntent>>,
}

impl ControllingSessionManager {
    /// 打开（或复用同名 ref 的）远程会话意图。要求当前存在活租约，并把该租约
    /// 与协商能力固化进意图；模式与模型来源按首版能力开关失败关闭。
    /// The created flag is decided under the same intent lock as open; callers
    /// must not infer ownership from a separate state read when compensating.
    pub(crate) fn open_with_outcome(
        &self,
        remote_ref: Option<&str>,
        title: &str,
        mode: &str,
        lease: &LeaseContext,
        negotiated_capabilities: &[String],
    ) -> Result<(ControllingSessionProjection, bool)> {
        self.open_internal(remote_ref, title, mode, lease, negotiated_capabilities)
    }

    fn open_internal(
        &self,
        remote_ref: Option<&str>,
        title: &str,
        mode: &str,
        lease: &LeaseContext,
        negotiated_capabilities: &[String],
    ) -> Result<(ControllingSessionProjection, bool)> {
        if mode != SESSION_MODE_CHAT {
            return Err(SessionError::new("session_mode_unavailable"));
        }
        let title = title.trim();
        if title.is_empty() || title.chars().count() > MAX_TITLE_CHARS {
            return Err(SessionError::new("session_title_invalid"));
        }
        let session_ref = match remote_ref {
            Some(value) if is_valid_identifier(value) => value.to_owned(),
            Some(_) => return Err(SessionError::new("session_reference_invalid")),
            None => format!("session-ref-{}", Uuid::new_v4().simple()),
        };
        let mut intents = self.intents.lock();
        if let Some(existing) = intents
            .iter_mut()
            .find(|intent| intent.session_ref == session_ref)
        {
            ensure_context(existing, lease)?;
            if existing.session_id != session_ref {
                return Err(SessionError::new("session_reference_conflict"));
            }
            return Ok((existing.projection(), false));
        }
        if intents.len() >= MAX_SESSION_INTENTS {
            return Err(SessionError::new("session_capacity"));
        }
        let intent = SessionIntent {
            session_id: session_ref.clone(),
            session_ref,
            title: title.to_owned(),
            mode: SESSION_MODE_CHAT.to_owned(),
            model_source: MODEL_SOURCE_CONTROLLED.to_owned(),
            lease: lease.clone(),
            negotiated_capabilities: negotiated_capabilities.to_vec(),
            capability_revision: None,
            capability_valid_for_ms: None,
            capability_projection: None,
            capability_deadline: None,
            capability_error: None,
            last_capability_request_id: None,
            pending_capability_requests: Vec::new(),
            modes: vec![SESSION_MODE_CHAT.to_owned()],
            workspace_ids: Vec::new(),
            tasks: BTreeMap::new(),
            request_counter: 0,
        };
        let projection = intent.projection();
        intents.push(intent);
        Ok((projection, true))
    }

    pub(crate) fn state(
        &self,
        remote_ref: &str,
        lease: &LeaseContext,
    ) -> Result<ControllingSessionProjection> {
        let mut intents = self.intents.lock();
        let intent = find_intent(&mut intents, remote_ref)?;
        ensure_context(intent, lease)?;
        Ok(intent.projection())
    }

    /// Read only A's authenticated, still-live capability projection for the
    /// exact remote ref and lease. B's local CapabilitySnapshotManager is not
    /// consulted and no raw protocol payload crosses to the WebView.
    pub(crate) fn capability_projection(
        &self,
        remote_ref: &str,
        lease: &LeaseContext,
    ) -> Result<ControllerCapabilityProjection> {
        let mut intents = self.intents.lock();
        let intent = find_intent(&mut intents, remote_ref)?;
        ensure_context(intent, lease)?;
        let deadline = intent
            .capability_deadline
            .ok_or_else(|| SessionError::new("capability_snapshot_required"))?;
        if Instant::now() >= deadline {
            return Err(SessionError::new("capability_snapshot_expired"));
        }
        let projection = intent
            .capability_projection
            .as_ref()
            .ok_or_else(|| SessionError::new("capability_snapshot_required"))?;
        if projection.connection_id != lease.connection_id
            || projection.controller_lease_id != lease.controller_lease_id
            || projection.lease_epoch != lease.lease_epoch
            || projection.relay_epoch != lease.relay_epoch
            || projection.remote_control_generation != lease.remote_control_generation
            || Some(projection.capability_revision) != intent.capability_revision
        {
            return Err(SessionError::new("session_context_stale"));
        }
        Ok(projection.clone())
    }

    /// 关闭一个会话意图；不存在的 ref 视为已经关闭（幂等）。
    pub(crate) fn close(&self, remote_ref: &str) -> Result<()> {
        let mut intents = self.intents.lock();
        intents.retain(|intent| intent.session_ref != remote_ref);
        Ok(())
    }

    /// 断开/停止所有连接上下文时清空全部会话意图（内存态，无持久化）。
    pub(crate) fn clear(&self) {
        self.intents.lock().clear();
    }

    /// 构造 `capability_snapshot_request`：以本地已知 revision 作为提示。
    pub(crate) fn capability_request(
        &self,
        remote_ref: &str,
        lease: &LeaseContext,
    ) -> Result<ControllingIntentProjection> {
        let mut intents = self.intents.lock();
        let intent = find_intent(&mut intents, remote_ref)?;
        ensure_context(intent, lease)?;
        let request_id = intent.next_request_id();
        let payload = build_capability_snapshot_request(intent, &request_id)?;
        intent.capability_error = None;
        intent.pending_capability_requests.push(request_id.clone());
        if intent.pending_capability_requests.len() > MAX_PENDING_CAPABILITY_REQUESTS {
            intent.pending_capability_requests.remove(0);
        }
        Ok(intent_projection(request_id, intent, None, None, payload))
    }

    /// Stage the fully validated response before confirming the B-owned Session
    /// mapping. Only after the mapping writer succeeds is the capability applied.
    pub(crate) fn apply_capability_snapshot_for_lease_with_mapping(
        &self,
        lease: &LeaseContext,
        payload: &Value,
        confirm_mapping: impl FnOnce(&str, &str) -> Result<()>,
    ) -> Result<ControllingSessionProjection> {
        let mut intents = self.intents.lock();
        let intent = find_capability_response_intent(&mut intents, lease, payload)?;
        let request_id = response_request_id(payload)?;
        if !intent
            .pending_capability_requests
            .iter()
            .any(|pending| pending == request_id)
        {
            return Err(SessionError::new("capability_response_unexpected"));
        }
        let controlled_session_id = payload
            .get("controlled_session_id")
            .and_then(Value::as_str)
            .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?;
        let mut staged = intent.clone();
        let projection = apply_snapshot_locked(&mut staged, payload, true)?;
        confirm_mapping(&intent.session_ref, controlled_session_id)?;
        staged.pending_capability_requests.clear();
        *intent = staged;
        Ok(projection)
    }

    pub(crate) fn apply_capability_error_for_lease(
        &self,
        lease: &LeaseContext,
        payload: &Value,
    ) -> Result<ControllingSessionProjection> {
        let mut intents = self.intents.lock();
        let intent = find_capability_response_intent(&mut intents, lease, payload)?;
        let validated = validate_inbound_payload(intent, payload, "capability_snapshot_error")?;
        let request_id = response_request_id(&validated)?;
        if intent
            .pending_capability_requests
            .last()
            .map(String::as_str)
            != Some(request_id)
        {
            intent
                .pending_capability_requests
                .retain(|pending| pending != request_id);
            return Err(SessionError::new("capability_response_stale"));
        }
        let code = validated
            .get("code")
            .and_then(Value::as_str)
            .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?;
        intent.pending_capability_requests.clear();
        intent.capability_revision = None;
        intent.capability_valid_for_ms = None;
        intent.capability_projection = None;
        intent.capability_deadline = None;
        intent.capability_error = Some(code.to_owned());
        intent.last_capability_request_id = Some(request_id.to_owned());
        intent.modes = vec![SESSION_MODE_CHAT.to_owned()];
        intent.workspace_ids.clear();
        Ok(intent.projection())
    }

    /// Relay 在已连接阶段拒绝业务帧时，外层错误帧不携带加密载荷里的 request_id。
    /// 只有当前租约恰好存在一个待处理能力会话时才允许关联；并发会话保持原状，
    /// 避免把传输层错误误投影到错误的会话。
    pub(crate) fn fail_pending_capability_for_lease(
        &self,
        lease: &LeaseContext,
        code: &str,
    ) -> Result<ControllingSessionProjection> {
        let mut intents = self.intents.lock();
        let mut matching = intents.iter_mut().filter(|intent| {
            &intent.lease == lease && !intent.pending_capability_requests.is_empty()
        });
        let intent = matching
            .next()
            .ok_or_else(|| SessionError::new("capability_response_unexpected"))?;
        if matching.next().is_some() {
            return Err(SessionError::new("capability_response_ambiguous"));
        }
        let request_id = intent
            .pending_capability_requests
            .last()
            .cloned()
            .ok_or_else(|| SessionError::new("capability_response_unexpected"))?;
        intent.pending_capability_requests.clear();
        intent.capability_revision = None;
        intent.capability_valid_for_ms = None;
        intent.capability_projection = None;
        intent.capability_deadline = None;
        intent.capability_error = Some(code.to_owned());
        intent.last_capability_request_id = Some(request_id);
        intent.modes = vec![SESSION_MODE_CHAT.to_owned()];
        intent.workspace_ids.clear();
        Ok(intent.projection())
    }

    /// 当前租约上的会话引用（心跳/恢复等按租约定位的场景使用）。
    pub(crate) fn session_ref_for_lease(&self, lease: &LeaseContext) -> Option<String> {
        self.intents
            .lock()
            .iter()
            .find(|intent| &intent.lease == lease)
            .map(|intent| intent.session_ref.clone())
    }

    /// 构造 `task_submit`：正文来自 UI，摘要与全部协议字段由 Rust 生成。
    pub(crate) fn task_intent(
        &self,
        remote_ref: &str,
        text: &str,
        lease: &LeaseContext,
    ) -> Result<ControllingIntentProjection> {
        self.task_intent_with_model(remote_ref, text, lease, MODEL_SOURCE_CONTROLLED, |_| {
            Ok(None)
        })
    }

    /// 构造带任务级模型来源的 `task_submit`。临时模型包必须由调用方在这里通过
    /// `task_id` 现建，确保包绑定与任务标识完全一致，且不会经过前端明文投影。
    pub(crate) fn task_intent_with_model<F>(
        &self,
        remote_ref: &str,
        text: &str,
        lease: &LeaseContext,
        model_source: &str,
        package_for_task: F,
    ) -> Result<ControllingIntentProjection>
    where
        F: FnOnce(&str) -> Result<Option<Value>>,
    {
        let text = text.trim();
        if text.is_empty() || text.len() > MAX_TASK_INPUT_TEXT_BYTES {
            return Err(SessionError::new("task_input_invalid"));
        }
        if !matches!(
            model_source,
            MODEL_SOURCE_CONTROLLED | MODEL_SOURCE_TEMPORARY
        ) {
            return Err(SessionError::new("task_model_source_invalid"));
        }
        let mut intents = self.intents.lock();
        let intent = find_intent(&mut intents, remote_ref)?;
        ensure_context(intent, lease)?;
        // 未取到能力快照前不构造任务：revision 是任务接纳的准入条件。
        if intent.capability_revision.is_none() {
            return Err(SessionError::new("capability_snapshot_required"));
        }
        if model_source == MODEL_SOURCE_TEMPORARY
            && !intent
                .negotiated_capabilities
                .iter()
                .any(|capability| capability == "temporary_model_package_v1")
        {
            return Err(SessionError::new("temporary_model_capability_required"));
        }
        if intent.tasks.len() >= MAX_TRACKED_TASKS {
            return Err(SessionError::new("task_capacity"));
        }
        let request_id = intent.next_request_id();
        let task_id = format!("task-{}", Uuid::new_v4().simple());
        let client_request_id = format!("client-request-{}", Uuid::new_v4().simple());
        let temporary_model_package = package_for_task(&task_id)?;
        let payload = build_task_submit(
            intent,
            &request_id,
            &task_id,
            &client_request_id,
            text,
            model_source,
            temporary_model_package,
        )?;
        intent.model_source = model_source.to_owned();
        intent
            .tasks
            .insert(task_id.clone(), client_request_id.clone());
        Ok(intent_projection(
            request_id,
            intent,
            Some(task_id),
            Some(client_request_id),
            payload,
        ))
    }

    /// 构造 `task_cancel`：只允许取消本会话已构造过的任务意图。
    pub(crate) fn task_cancel_intent(
        &self,
        remote_ref: &str,
        task_id: &str,
        lease: &LeaseContext,
    ) -> Result<ControllingIntentProjection> {
        let mut intents = self.intents.lock();
        let intent = find_intent(&mut intents, remote_ref)?;
        ensure_context(intent, lease)?;
        if !intent.tasks.contains_key(task_id) {
            return Err(SessionError::new("task_not_found"));
        }
        let request_id = intent.next_request_id();
        let payload = build_task_cancel(intent, &request_id, task_id)?;
        Ok(intent_projection(
            request_id,
            intent,
            Some(task_id.to_owned()),
            None,
            payload,
        ))
    }
}

fn find_intent<'a>(
    intents: &'a mut Vec<SessionIntent>,
    remote_ref: &str,
) -> Result<&'a mut SessionIntent> {
    intents
        .iter_mut()
        .find(|intent| intent.session_ref == remote_ref)
        .ok_or_else(|| SessionError::new("session_not_found"))
}

fn response_request_id(payload: &Value) -> Result<&str> {
    payload
        .get("request_id")
        .and_then(Value::as_str)
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))
}

fn find_capability_response_intent<'a>(
    intents: &'a mut [SessionIntent],
    lease: &LeaseContext,
    payload: &Value,
) -> Result<&'a mut SessionIntent> {
    let request_id = response_request_id(payload)?;
    if let Some(session_id) = payload.get("session_id").and_then(Value::as_str) {
        return intents
            .iter_mut()
            .find(|intent| {
                &intent.lease == lease
                    && intent.session_id == session_id
                    && intent
                        .pending_capability_requests
                        .iter()
                        .any(|pending| pending == request_id)
            })
            .ok_or_else(|| SessionError::new("session_not_found"));
    }
    let mut matches = intents.iter_mut().filter(|intent| {
        &intent.lease == lease
            && intent
                .pending_capability_requests
                .iter()
                .any(|pending| pending == request_id)
    });
    let intent = matches
        .next()
        .ok_or_else(|| SessionError::new("session_not_found"))?;
    if matches.next().is_some() {
        return Err(SessionError::new("capability_request_ambiguous"));
    }
    Ok(intent)
}

/// 校验并落地一份能力快照：形状/角色/租约由冻结运行时校验保证，revision 必须
/// 单调不减，模式不能为空，工作区列表有界。
fn apply_snapshot_locked(
    intent: &mut SessionIntent,
    payload: &Value,
    authenticated_ingress: bool,
) -> Result<ControllingSessionProjection> {
    let validated = validate_inbound_payload(intent, payload, "capability_snapshot")?;
    let revision = validated
        .get("capability_revision")
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<u64>().ok())
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?;
    if revision == 0 {
        return Err(SessionError::new("capability_snapshot_invalid"));
    }
    if intent
        .capability_revision
        .is_some_and(|known| revision < known)
    {
        return Err(SessionError::new("capability_snapshot_stale"));
    }
    let modes = string_list(validated.get("modes"), MAX_MODES)?;
    if modes.is_empty() {
        return Err(SessionError::new("capability_snapshot_invalid"));
    }
    let mut workspace_ids = Vec::new();
    let mut workspaces = Vec::new();
    for workspace in validated
        .get("workspaces")
        .and_then(Value::as_array)
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?
    {
        let workspace_id = workspace
            .get("workspace_id")
            .and_then(Value::as_str)
            .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?;
        workspace_ids.push(workspace_id.to_owned());
        workspaces.push(ControllerWorkspaceDescriptor {
            workspace_id: workspace_id.to_owned(),
            display_name: descriptor_string(workspace, "display_name")?,
            code_allowed: workspace
                .get("code_allowed")
                .and_then(Value::as_bool)
                .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?,
        });
        if workspace_ids.len() > MAX_WORKSPACES {
            return Err(SessionError::new("capability_snapshot_invalid"));
        }
    }
    let tools = validated
        .get("tools")
        .and_then(Value::as_array)
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?
        .iter()
        .map(|value| {
            Ok(ControllerToolDescriptor {
                tool_id: descriptor_string(value, "tool_id")?,
                display_name: descriptor_string(value, "display_name")?,
            })
        })
        .collect::<Result<Vec<_>>>()?;
    let skills = validated
        .get("skills")
        .and_then(Value::as_array)
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?
        .iter()
        .map(|value| {
            Ok(ControllerSkillDescriptor {
                skill_id: descriptor_string(value, "skill_id")?,
                display_name: descriptor_string(value, "display_name")?,
                scopes: string_list(value.get("scopes"), MAX_MODES)?,
            })
        })
        .collect::<Result<Vec<_>>>()?;
    let valid_for_ms = validated
        .get("valid_for_ms")
        .and_then(Value::as_str)
        .map(str::parse::<u64>)
        .transpose()
        .map_err(|_| SessionError::new("capability_snapshot_invalid"))?
        .unwrap_or(MAX_A_CAPABILITY_TTL_MS)
        .min(MAX_A_CAPABILITY_TTL_MS);
    if valid_for_ms == 0 {
        return Err(SessionError::new("capability_snapshot_invalid"));
    }
    let deadline = Instant::now()
        .checked_add(Duration::from_millis(valid_for_ms))
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?;
    let now_ms = u64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| SessionError::new("capability_snapshot_invalid"))?
            .as_millis(),
    )
    .map_err(|_| SessionError::new("capability_snapshot_invalid"))?;
    let expires_at_ms = now_ms
        .checked_add(valid_for_ms)
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?;
    let capability = ControllerCapabilityProjection {
        remote_session_ref: intent.session_ref.clone(),
        connection_id: intent.lease.connection_id.clone(),
        controller_lease_id: intent.lease.controller_lease_id.clone(),
        lease_epoch: intent.lease.lease_epoch.clone(),
        relay_epoch: intent.lease.relay_epoch.clone(),
        remote_control_generation: intent.lease.remote_control_generation.clone(),
        capability_revision: revision,
        expires_at_ms,
        modes: modes.clone(),
        tools,
        skills,
        workspaces,
    };
    if intent.capability_revision == Some(revision) {
        if !authenticated_ingress {
            return Ok(intent.projection());
        }
        let old = intent
            .capability_projection
            .as_ref()
            .ok_or_else(|| SessionError::new("capability_snapshot_stale"))?;
        if old.modes != capability.modes
            || old.tools != capability.tools
            || old.skills != capability.skills
            || old.workspaces != capability.workspaces
        {
            return Err(SessionError::new("capability_snapshot_stale"));
        }
        return Ok(intent.projection());
    }
    intent.capability_revision = Some(revision);
    intent.capability_valid_for_ms = Some(valid_for_ms);
    intent.capability_projection = authenticated_ingress.then_some(capability);
    intent.capability_deadline = authenticated_ingress.then_some(deadline);
    intent.capability_error = None;
    intent.last_capability_request_id = Some(response_request_id(&validated)?.to_owned());
    intent.modes = modes;
    intent.workspace_ids = workspace_ids;
    Ok(intent.projection())
}

fn descriptor_string(value: &Value, key: &str) -> Result<String> {
    value
        .get(key)
        .and_then(Value::as_str)
        .map(str::to_owned)
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))
}

/// 会话绑定校验：lease 的五元组必须与意图创建时完全一致，否则视为陈旧
/// 上下文（例如重连后 lease_epoch/generation 变化）。
fn ensure_context(intent: &SessionIntent, lease: &LeaseContext) -> Result<()> {
    if &intent.lease != lease {
        return Err(SessionError::new("session_context_stale"));
    }
    Ok(())
}

fn intent_projection(
    request_id: String,
    intent: &SessionIntent,
    task_id: Option<String>,
    client_request_id: Option<String>,
    payload: Value,
) -> ControllingIntentProjection {
    ControllingIntentProjection {
        request_id,
        session_id: intent.session_id.clone(),
        task_id,
        client_request_id,
        payload,
    }
}

fn build_capability_snapshot_request(intent: &SessionIntent, request_id: &str) -> Result<Value> {
    let payload = json!({
        "v": 1,
        "type": "capability_snapshot_request",
        "connection_id": intent.lease.connection_id,
        "controller_lease_id": intent.lease.controller_lease_id,
        "lease_epoch": intent.lease.lease_epoch,
        "relay_epoch": intent.lease.relay_epoch,
        "remote_control_generation": intent.lease.remote_control_generation,
        "request_id": request_id,
        "session_id": intent.session_id,
        "known_revision": intent.capability_revision.unwrap_or(0).to_string(),
    });
    validate_outbound_payload(intent, &payload)
}

fn build_task_submit(
    intent: &SessionIntent,
    request_id: &str,
    task_id: &str,
    client_request_id: &str,
    text: &str,
    model_source: &str,
    temporary_model_package: Option<Value>,
) -> Result<Value> {
    let revision = intent
        .capability_revision
        .ok_or_else(|| SessionError::new("capability_snapshot_required"))?;
    let input = json!({
        "text": text,
        "attachment_ids": Vec::<String>::new(),
        "skill_ids": Vec::<String>::new(),
    });
    let mut payload = json!({
        "v": 1,
        "type": "task_submit",
        "connection_id": intent.lease.connection_id,
        "controller_lease_id": intent.lease.controller_lease_id,
        "lease_epoch": intent.lease.lease_epoch,
        "relay_epoch": intent.lease.relay_epoch,
        "remote_control_generation": intent.lease.remote_control_generation,
        "request_id": request_id,
        "task_id": task_id,
        "client_request_id": client_request_id,
        "session_id": intent.session_id,
        "capability_revision": revision.to_string(),
        "model_source": model_source,
        "mode": intent.mode,
        "input_digest": task_input_digest(text),
        "input": input,
    });
    if let Some(package) = temporary_model_package {
        payload["temporary_model_package"] = package;
    }
    validate_outbound_payload(intent, &payload)
}

fn build_task_cancel(intent: &SessionIntent, request_id: &str, task_id: &str) -> Result<Value> {
    let payload =
        build_bound_task_cancel_payload(&intent.lease, &intent.session_id, request_id, task_id)?;
    validate_outbound_payload(intent, &payload)
}

/// Pure cancellation frame builder for the SessionStore-backed A task path.
/// The caller must first prove an attempted task in the current RemoteProxy
/// binding; this helper deliberately owns no parallel task registry.
pub(crate) fn build_bound_task_cancel_payload(
    lease: &LeaseContext,
    session_id: &str,
    request_id: &str,
    task_id: &str,
) -> Result<Value> {
    if !is_valid_identifier(session_id)
        || !is_valid_identifier(request_id)
        || !is_valid_identifier(task_id)
    {
        return Err(SessionError::new("task_request_invalid"));
    }
    Ok(json!({
        "v": 1,
        "type": "task_cancel",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": request_id,
        "task_id": task_id,
        "session_id": session_id,
    }))
}

/// 输入摘要：域分离前缀 + 逐字段长度前缀，同参同摘要、异参不同摘要。冻结协议
/// 只规定 64 位小写 hex 的形状，算法由本端定义；B 仅用于同参重放比对。
pub(crate) fn task_input_digest(text: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(TASK_DIGEST_DOMAIN);
    for field in [text.as_bytes(), b"".as_slice(), b"".as_slice()] {
        hasher.update((field.len() as u64).to_be_bytes());
        hasher.update(field);
    }
    let digest = hasher.finalize();
    digest.iter().map(|byte| format!("{byte:02x}")).collect()
}

fn validate_outbound_payload(intent: &SessionIntent, payload: &Value) -> Result<Value> {
    let encoded =
        serde_json::to_string(payload).map_err(|_| SessionError::new("task_request_invalid"))?;
    validate_controller_runtime_payload_json(
        &encoded,
        SenderRole::ControllerA,
        Some(&intent.runtime_context()),
    )
    .map_err(|error| match error.code {
        "unsupported_capability" => SessionError::new("capability_not_negotiated"),
        "stale_context" => SessionError::new("session_context_stale"),
        _ => SessionError::new("task_request_invalid"),
    })
}

fn validate_inbound_payload(intent: &SessionIntent, payload: &Value, kind: &str) -> Result<Value> {
    let encoded = serde_json::to_string(payload)
        .map_err(|_| SessionError::new("capability_snapshot_invalid"))?;
    let validated = validate_controller_runtime_payload_json(
        &encoded,
        SenderRole::ControlledB,
        Some(&intent.runtime_context()),
    )
    .map_err(|_| SessionError::new("capability_snapshot_invalid"))?;
    if validated.get("type").and_then(Value::as_str) != Some(kind) {
        return Err(SessionError::new("capability_snapshot_invalid"));
    }
    Ok(validated)
}

fn string_list(value: Option<&Value>, limit: usize) -> Result<Vec<String>> {
    let items = value
        .and_then(Value::as_array)
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?;
    if items.len() > limit {
        return Err(SessionError::new("capability_snapshot_invalid"));
    }
    items
        .iter()
        .map(|item| {
            item.as_str()
                .map(str::to_owned)
                .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    // Test construction still traverses the production open outcome and the
    // pending-request + validated B-mapping capability admission path.
    trait SessionTestSetup {
        fn open_for_test(
            &self,
            remote_ref: Option<&str>,
            title: &str,
            mode: &str,
            lease: &LeaseContext,
            capabilities: &[String],
        ) -> Result<ControllingSessionProjection>;
        fn apply_checked_snapshot_for_test(
            &self,
            remote_ref: &str,
            payload: &Value,
            lease: &LeaseContext,
        ) -> Result<ControllingSessionProjection>;
    }

    impl SessionTestSetup for ControllingSessionManager {
        fn open_for_test(
            &self,
            remote_ref: Option<&str>,
            title: &str,
            mode: &str,
            lease: &LeaseContext,
            capabilities: &[String],
        ) -> Result<ControllingSessionProjection> {
            self.open_with_outcome(remote_ref, title, mode, lease, capabilities)
                .map(|(projection, _created)| projection)
        }

        fn apply_checked_snapshot_for_test(
            &self,
            remote_ref: &str,
            payload: &Value,
            lease: &LeaseContext,
        ) -> Result<ControllingSessionProjection> {
            let request = self.capability_request(remote_ref, lease)?;
            let mut response = payload.clone();
            response["request_id"] = json!(request.request_id);
            response["session_id"] = json!(request.session_id);
            response["controlled_session_id"] = json!("b-session-1");
            self.apply_capability_snapshot_for_lease_with_mapping(
                lease,
                &response,
                |actual_ref, b_session| {
                    if actual_ref != remote_ref || b_session != "b-session-1" {
                        return Err(SessionError::new("remote_mapping_mismatch"));
                    }
                    Ok(())
                },
            )
        }
    }

    fn lease() -> LeaseContext {
        LeaseContext {
            connection_id: "connection-0001".into(),
            controller_lease_id: "lease-0001".into(),
            lease_epoch: "1".into(),
            relay_epoch: "relay-epoch-0001".into(),
            remote_control_generation: "generation-0001".into(),
        }
    }

    fn capabilities() -> Vec<String> {
        vec![
            "encrypted_frames_v1".to_owned(),
            "pairing_v1".to_owned(),
            "task_v1".to_owned(),
            "temporary_model_package_v1".to_owned(),
        ]
    }

    /// 冻结形状的 B 端能力快照（字段顺序与体积都不影响本端校验）。
    fn snapshot(revision: u64) -> Value {
        json!({
            "v": 1,
            "type": "capability_snapshot",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "1",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-0001",
            "request_id": "request-snapshot-0001",
            "capability_revision": revision.to_string(),
            "expires_at_ms": "30000",
            "modes": ["chat"],
            "tools": [],
            "skills": [],
            "workspaces": [
                {"workspace_id": "workspace-0001", "display_name": "Projects", "code_allowed": true}
            ],
        })
    }

    #[test]
    fn session_open_binds_the_live_lease_and_only_opens_chat() {
        let sessions = ControllingSessionManager::default();
        let projection = sessions
            .open_for_test(None, "  Weekly report  ", "chat", &lease(), &capabilities())
            .unwrap();
        assert_eq!(projection.title, "Weekly report");
        assert_eq!(projection.mode, "chat");
        assert_eq!(projection.model_source, "controlled_device");
        assert_eq!(projection.connection_id, "connection-0001");
        assert_eq!(projection.controller_lease_id, "lease-0001");
        assert_eq!(projection.lease_epoch, "1");
        assert_eq!(projection.session_id, projection.session_ref);
        assert_eq!(projection.relay_epoch, "relay-epoch-0001");
        assert_eq!(projection.remote_control_generation, "generation-0001");
        let serialized = serde_json::to_value(&projection).unwrap();
        assert_eq!(serialized["controllerLeaseId"], "lease-0001");
        assert_eq!(serialized["relayEpoch"], "relay-epoch-0001");
        assert!(is_valid_identifier(&projection.session_id));
        assert_eq!(projection.capability_revision, None);
        assert!(projection.temporary_model_available);
        assert_eq!(projection.modes, vec!["chat".to_owned()]);

        let controlled_only = sessions
            .open_for_test(
                Some("session-ref-controlled-only"),
                "Controlled only",
                "chat",
                &lease(),
                &[
                    "encrypted_frames_v1".into(),
                    "pairing_v1".into(),
                    "task_v1".into(),
                ],
            )
            .unwrap();
        assert!(!controlled_only.temporary_model_available);

        // 首版能力开关：代码模式、空标题与非法引用都失败关闭。
        assert_eq!(
            sessions
                .open_for_test(None, "Code", "code", &lease(), &capabilities())
                .unwrap_err()
                .code,
            "session_mode_unavailable"
        );
        assert_eq!(
            sessions
                .open_for_test(None, "   ", "chat", &lease(), &capabilities())
                .unwrap_err()
                .code,
            "session_title_invalid"
        );
        assert_eq!(
            sessions
                .open_for_test(Some("bad ref"), "Title", "chat", &lease(), &capabilities())
                .unwrap_err()
                .code,
            "session_reference_invalid"
        );
        // 同名 ref 复用同一意图（幂等打开）。
        let reopened = sessions
            .open_for_test(
                Some(&projection.session_ref),
                "Weekly report",
                "chat",
                &lease(),
                &capabilities(),
            )
            .unwrap();
        assert_eq!(reopened.session_id, projection.session_id);
    }

    #[test]
    fn capability_request_and_snapshot_apply_are_lease_bound_and_monotonic() {
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_for_test(None, "Session", "chat", &lease(), &capabilities())
            .unwrap();
        let request = sessions
            .capability_request(&session.session_ref, &lease())
            .unwrap();
        assert_eq!(request.payload["type"], "capability_snapshot_request");
        assert_eq!(request.payload["known_revision"], "0");
        assert_eq!(request.payload["session_id"], session.session_id);

        let applied = sessions
            .apply_checked_snapshot_for_test(&session.session_ref, &snapshot(4), &lease())
            .unwrap();
        assert_eq!(applied.capability_revision, Some(4));
        assert_eq!(applied.workspace_count, 1);

        // 同 revision 幂等；回退失败关闭。
        let idempotent = sessions
            .apply_checked_snapshot_for_test(&session.session_ref, &snapshot(4), &lease())
            .unwrap();
        assert_eq!(idempotent.capability_revision, Some(4));
        assert_eq!(
            sessions
                .apply_checked_snapshot_for_test(&session.session_ref, &snapshot(3), &lease())
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        // 跨 lease 的快照必须失败关闭（lease_epoch 变化的旧快照）。
        let mut foreign = snapshot(5);
        foreign["lease_epoch"] = json!("2");
        assert_eq!(
            sessions
                .apply_checked_snapshot_for_test(&session.session_ref, &foreign, &lease())
                .unwrap_err()
                .code,
            "capability_snapshot_invalid"
        );
        // 跨连接（A 端 lease 已变化）同样失败关闭。
        let mut other_lease = lease();
        other_lease.remote_control_generation = "generation-0002".into();
        assert_eq!(
            sessions
                .apply_checked_snapshot_for_test(&session.session_ref, &snapshot(6), &other_lease)
                .unwrap_err()
                .code,
            "session_context_stale"
        );
    }

    #[test]
    fn capability_responses_are_request_and_session_bound() {
        let sessions = ControllingSessionManager::default();
        let first = sessions
            .open_for_test(
                Some("session-ref-first"),
                "First",
                "chat",
                &lease(),
                &capabilities(),
            )
            .unwrap();
        let second = sessions
            .open_for_test(
                Some("session-ref-second"),
                "Second",
                "chat",
                &lease(),
                &capabilities(),
            )
            .unwrap();
        let request = sessions
            .capability_request(&second.session_ref, &lease())
            .unwrap();
        let mut response = snapshot(5);
        response["request_id"] = json!(request.request_id);
        response["session_id"] = json!(second.session_id);
        response["controlled_session_id"] = json!("b-session-1");

        let applied = sessions
            .apply_capability_snapshot_for_lease_with_mapping(
                &lease(),
                &response,
                |remote_ref, b| {
                    assert_eq!(remote_ref, second.session_ref);
                    assert_eq!(b, "b-session-1");
                    Ok(())
                },
            )
            .unwrap();
        assert_eq!(applied.session_ref, second.session_ref);
        assert_eq!(applied.capability_revision, Some(5));
        assert_eq!(
            sessions
                .state(&first.session_ref, &lease())
                .unwrap()
                .capability_revision,
            None
        );

        let retry = sessions
            .capability_request(&first.session_ref, &lease())
            .unwrap();
        let failure = json!({
            "v": 1,
            "type": "capability_snapshot_error",
            "connection_id": "connection-0001",
            "controller_lease_id": "lease-0001",
            "lease_epoch": "1",
            "relay_epoch": "relay-epoch-0001",
            "remote_control_generation": "generation-0001",
            "request_id": retry.request_id,
            "session_id": first.session_ref,
            "code": "capability_snapshot_unavailable",
        });
        let failed = sessions
            .apply_capability_error_for_lease(&lease(), &failure)
            .unwrap();
        assert_eq!(failed.session_ref, first.session_ref);
        assert_eq!(
            failed.capability_error.as_deref(),
            Some("capability_snapshot_unavailable")
        );
        assert_eq!(failed.capability_revision, None);
    }

    #[test]
    fn authenticated_a_capability_projection_is_allowlisted_ref_lease_revision_and_ttl_bound() {
        let sessions = ControllingSessionManager::default();
        let first = sessions
            .open_for_test(
                Some("ref-first"),
                "First",
                "chat",
                &lease(),
                &capabilities(),
            )
            .unwrap();
        let second = sessions
            .open_for_test(
                Some("ref-second"),
                "Second",
                "chat",
                &lease(),
                &capabilities(),
            )
            .unwrap();
        let request = sessions
            .capability_request(&first.session_ref, &lease())
            .unwrap();
        let mut response = snapshot(7);
        response["request_id"] = json!(request.request_id);
        response["session_id"] = json!(first.session_id);
        response["controlled_session_id"] = json!("b-session-1");
        response["tools"] = json!([{"tool_id":"tool-1","display_name":"Tool"}]);
        response["skills"] =
            json!([{"skill_id":"skill-1","display_name":"Skill","scopes":["chat"]}]);
        response["valid_for_ms"] = json!("30000");
        sessions
            .apply_capability_snapshot_for_lease_with_mapping(
                &lease(),
                &response,
                |remote_ref, b| {
                    assert_eq!(remote_ref, first.session_ref);
                    assert_eq!(b, "b-session-1");
                    Ok(())
                },
            )
            .unwrap();
        let projection = sessions
            .capability_projection(&first.session_ref, &lease())
            .unwrap();
        assert_eq!(projection.capability_revision, 7);
        assert_eq!(projection.tools[0].tool_id, "tool-1");
        assert_eq!(projection.skills[0].skill_id, "skill-1");
        assert_eq!(projection.skills[0].scopes, ["chat"]);
        assert_eq!(projection.workspaces[0].workspace_id, "workspace-0001");
        assert!(projection.expires_at_ms > 0);
        let encoded = serde_json::to_value(&projection).unwrap();
        let actual_keys: std::collections::BTreeSet<_> = encoded
            .as_object()
            .unwrap()
            .keys()
            .map(String::as_str)
            .collect();
        assert_eq!(
            actual_keys,
            [
                "capabilityRevision",
                "connectionId",
                "controllerLeaseId",
                "expiresAtMs",
                "leaseEpoch",
                "modes",
                "relayEpoch",
                "remoteControlGeneration",
                "remoteSessionRef",
                "skills",
                "tools",
                "workspaces",
            ]
            .into_iter()
            .collect()
        );
        assert!(encoded.get("requestId").is_none());
        assert!(encoded.get("sessionId").is_none());
        assert!(encoded.get("peerId").is_none());
        assert!(encoded.get("credential").is_none());
        assert_eq!(encoded["tools"][0]["toolId"], "tool-1");
        assert_eq!(encoded["skills"][0]["scopes"], json!(["chat"]));
        assert_eq!(encoded["workspaces"][0]["codeAllowed"], true);
        assert_eq!(
            sessions
                .capability_projection(&second.session_ref, &lease())
                .unwrap_err()
                .code,
            "capability_snapshot_required"
        );
        let mut other_lease = lease();
        other_lease.controller_lease_id = "lease-other".into();
        assert_eq!(
            sessions
                .capability_projection(&first.session_ref, &other_lease)
                .unwrap_err()
                .code,
            "session_context_stale"
        );
        let retry = sessions
            .capability_request(&first.session_ref, &lease())
            .unwrap();
        let mut conflicting_same_revision = response.clone();
        conflicting_same_revision["request_id"] = json!(retry.request_id);
        conflicting_same_revision["tools"] =
            json!([{"tool_id":"tool-2","display_name":"Other Tool"}]);
        assert_eq!(
            sessions
                .apply_capability_snapshot_for_lease_with_mapping(
                    &lease(),
                    &conflicting_same_revision,
                    |remote_ref, b| {
                        assert_eq!(remote_ref, first.session_ref);
                        assert_eq!(b, "b-session-1");
                        Ok(())
                    }
                )
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        let mut stale = response.clone();
        stale["request_id"] = json!(retry.request_id);
        stale["capability_revision"] = json!("6");
        assert_eq!(
            sessions
                .apply_capability_snapshot_for_lease_with_mapping(
                    &lease(),
                    &stale,
                    |remote_ref, b| {
                        assert_eq!(remote_ref, first.session_ref);
                        assert_eq!(b, "b-session-1");
                        Ok(())
                    }
                )
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        {
            let mut intents = sessions.intents.lock();
            let intent = intents
                .iter_mut()
                .find(|item| item.session_ref == first.session_ref)
                .unwrap();
            intent.capability_deadline = Some(Instant::now() - Duration::from_millis(1));
        }
        assert_eq!(
            sessions
                .capability_projection(&first.session_ref, &lease())
                .unwrap_err()
                .code,
            "capability_snapshot_expired"
        );
    }

    #[test]
    fn relay_failure_is_only_associated_when_one_capability_session_is_pending() {
        let sessions = ControllingSessionManager::default();
        let first = sessions
            .open_for_test(
                Some("session-ref-first"),
                "First",
                "chat",
                &lease(),
                &capabilities(),
            )
            .unwrap();
        let first_request = sessions
            .capability_request(&first.session_ref, &lease())
            .unwrap();
        let failed = sessions
            .fail_pending_capability_for_lease(&lease(), "capability_snapshot_unavailable")
            .unwrap();
        assert_eq!(failed.session_ref, first.session_ref);
        assert_eq!(failed.capability_request_id, Some(first_request.request_id));
        assert_eq!(
            failed.capability_error.as_deref(),
            Some("capability_snapshot_unavailable")
        );

        let second = sessions
            .open_for_test(
                Some("session-ref-second"),
                "Second",
                "chat",
                &lease(),
                &capabilities(),
            )
            .unwrap();
        sessions
            .capability_request(&first.session_ref, &lease())
            .unwrap();
        sessions
            .capability_request(&second.session_ref, &lease())
            .unwrap();
        assert_eq!(
            sessions
                .fail_pending_capability_for_lease(&lease(), "capability_snapshot_unavailable",)
                .unwrap_err()
                .code,
            "capability_response_ambiguous"
        );
        assert_eq!(
            sessions
                .state(&first.session_ref, &lease())
                .unwrap()
                .capability_error,
            None
        );
        assert_eq!(
            sessions
                .state(&second.session_ref, &lease())
                .unwrap()
                .capability_error,
            None
        );
    }

    #[test]
    fn task_intent_builds_a_frozen_payload_and_requires_a_snapshot() {
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_for_test(None, "Session", "chat", &lease(), &capabilities())
            .unwrap();
        // 未取到能力快照前不得构造任务。
        assert_eq!(
            sessions
                .task_intent(&session.session_ref, "hello", &lease())
                .unwrap_err()
                .code,
            "capability_snapshot_required"
        );
        assert_eq!(
            sessions
                .task_intent(&session.session_ref, "   ", &lease())
                .unwrap_err()
                .code,
            "task_input_invalid"
        );
        sessions
            .apply_checked_snapshot_for_test(&session.session_ref, &snapshot(4), &lease())
            .unwrap();

        let intent = sessions
            .task_intent(&session.session_ref, "  summarize the report  ", &lease())
            .unwrap();
        let payload = &intent.payload;
        assert_eq!(payload["type"], "task_submit");
        assert_eq!(payload["session_id"], session.session_id);
        assert_eq!(payload["capability_revision"], "4");
        assert_eq!(payload["model_source"], "controlled_device");
        assert_eq!(payload["mode"], "chat");
        assert_eq!(payload["input"]["text"], "summarize the report");
        assert_eq!(payload["input"]["attachment_ids"], json!([]));
        assert_eq!(
            payload["input_digest"].as_str().unwrap(),
            task_input_digest("summarize the report")
        );
        assert_eq!(intent.task_id.as_deref().unwrap(), payload["task_id"]);

        // 冻结字段的自我校验：任何字段缺失都会被 validate_outbound_payload 拦下。
        let mut broken = payload.clone();
        broken["input_digest"] = json!("not-a-digest");
        assert_eq!(
            validate_outbound_payload(
                &SessionIntent {
                    session_ref: session.session_ref.clone(),
                    session_id: session.session_id.clone(),
                    title: session.title.clone(),
                    mode: session.mode.clone(),
                    model_source: session.model_source.clone(),
                    lease: lease(),
                    negotiated_capabilities: capabilities(),
                    capability_revision: Some(4),
                    capability_valid_for_ms: Some(30_000),
                    capability_projection: None,
                    capability_deadline: None,
                    capability_error: None,
                    last_capability_request_id: None,
                    pending_capability_requests: Vec::new(),
                    modes: vec!["chat".into()],
                    workspace_ids: Vec::new(),
                    tasks: BTreeMap::new(),
                    request_counter: 0,
                },
                &broken,
            )
            .unwrap_err()
            .code,
            "task_request_invalid"
        );

        // 取消只能针对本会话已构造的任务。
        let cancel = sessions
            .task_cancel_intent(
                &session.session_ref,
                intent.task_id.as_deref().unwrap(),
                &lease(),
            )
            .unwrap();
        assert_eq!(cancel.payload["type"], "task_cancel");
        assert_eq!(cancel.payload["session_id"], session.session_id);
        assert_eq!(
            sessions
                .task_cancel_intent(&session.session_ref, "task-unknown", &lease())
                .unwrap_err()
                .code,
            "task_not_found"
        );
    }

    #[test]
    fn temporary_model_selection_binds_the_package_to_the_generated_task() {
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_for_test(None, "Session", "chat", &lease(), &capabilities())
            .unwrap();
        sessions
            .apply_checked_snapshot_for_test(&session.session_ref, &snapshot(4), &lease())
            .unwrap();
        let fixture: Value = serde_json::from_str(include_str!(
            "../../../../../../remote-control-relay/test/fixtures/controller-protocol-v1.json"
        ))
        .unwrap();
        let intent = sessions
            .task_intent_with_model(
                &session.session_ref,
                "use my model",
                &lease(),
                MODEL_SOURCE_TEMPORARY,
                |task_id| {
                    let mut package =
                        fixture["task_submit_vectors"]["temporary"]["temporary_model_package"]
                            .clone();
                    package["binding"]["task_id"] = json!(task_id);
                    Ok(Some(package))
                },
            )
            .unwrap();
        assert_eq!(intent.payload["model_source"], MODEL_SOURCE_TEMPORARY);
        assert_eq!(
            intent.payload["temporary_model_package"]["binding"]["task_id"],
            intent.task_id.unwrap()
        );
        assert_eq!(
            sessions
                .state(&session.session_ref, &lease())
                .unwrap()
                .model_source,
            MODEL_SOURCE_TEMPORARY
        );
    }

    #[test]
    fn capabilities_change_how_requests_fail_closed() {
        let sessions = ControllingSessionManager::default();
        // 未协商 task_v1 时，快照请求本身就非法（能力闭集失败关闭）。
        let without_task = vec!["encrypted_frames_v1".to_owned(), "pairing_v1".to_owned()];
        let session = sessions
            .open_for_test(None, "Session", "chat", &lease(), &without_task)
            .unwrap();
        assert_eq!(
            sessions
                .capability_request(&session.session_ref, &lease())
                .unwrap_err()
                .code,
            "capability_not_negotiated"
        );

        let legacy = ControllingSessionManager::default();
        let session = legacy
            .open_for_test(
                None,
                "Legacy",
                "chat",
                &lease(),
                &[
                    "encrypted_frames_v1".into(),
                    "pairing_v1".into(),
                    "task_v1".into(),
                ],
            )
            .unwrap();
        legacy
            .apply_checked_snapshot_for_test(&session.session_ref, &snapshot(1), &lease())
            .unwrap();
        assert_eq!(
            legacy
                .task_intent_with_model(
                    &session.session_ref,
                    "use my model",
                    &lease(),
                    MODEL_SOURCE_TEMPORARY,
                    |_| panic!("package must not be built without negotiated capability"),
                )
                .unwrap_err()
                .code,
            "temporary_model_capability_required"
        );
    }

    #[test]
    fn closing_and_clearing_drop_intents() {
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_for_test(None, "Session", "chat", &lease(), &capabilities())
            .unwrap();
        sessions.close(&session.session_ref).unwrap();
        sessions.close(&session.session_ref).unwrap();
        assert_eq!(
            sessions
                .state(&session.session_ref, &lease())
                .unwrap_err()
                .code,
            "session_not_found"
        );
        assert_eq!(
            sessions
                .task_intent(&session.session_ref, "hello", &lease())
                .unwrap_err()
                .code,
            "session_not_found"
        );
        // 会话容量上限：超出后拒绝而不是静默替换。
        for index in 0..MAX_SESSION_INTENTS {
            sessions
                .open_for_test(
                    Some(&format!("session-ref-{index}")),
                    "Session",
                    "chat",
                    &lease(),
                    &capabilities(),
                )
                .unwrap();
        }
        assert_eq!(
            sessions
                .open_for_test(None, "Session", "chat", &lease(), &capabilities())
                .unwrap_err()
                .code,
            "session_capacity"
        );
        sessions.clear();
        assert_eq!(
            sessions.state("session-ref-0", &lease()).unwrap_err().code,
            "session_not_found"
        );
    }

    #[test]
    fn input_digest_is_deterministic_and_parameter_bound() {
        let digest = task_input_digest("hello");
        assert_eq!(digest.len(), 64);
        assert_eq!(digest, task_input_digest("hello"));
        assert_ne!(digest, task_input_digest("hello "));
        assert_ne!(digest, task_input_digest("Hello"));
        assert_ne!(digest, task_input_digest(""));
    }
}
