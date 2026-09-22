//! A 端远程会话与任务意图（E2E-03）。
//!
//! 本模块只拥有「会话意图」这一份内存状态：它绑定当前 lease
//! （connection_id / controller_lease_id / lease_epoch / relay_epoch /
//! remote_control_generation）与协商能力，并按冻结 schema 构造 A 端发往 B 的
//! payload（能力快照请求、任务提交、任务取消）与输入摘要。前端只提交 UI 级
//! 字段（标题、正文、模式），不再拼装任何协议字段。
//!
//! 首版保守能力：仅开放 B 模型（`controlled_device`）的 `chat` 模式；代码
//! 模式、临时模型、附件与成果在此处失败关闭，等对应任务按能力开关打开。
//! 任何上下文不匹配（无连接、lease 变更、未取到能力快照）都失败关闭。

use std::collections::BTreeMap;

use parking_lot::Mutex;
use serde::Serialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use uuid::Uuid;

use super::protocol::{
    LeaseContext, MAX_TASK_INPUT_TEXT_BYTES, RuntimeValidationContext, SenderRole,
    is_valid_identifier, validate_controller_runtime_payload_json,
};

/// 首版唯一开放的会话模式（代码模式需要工作区授权与能力开关，属后续任务）。
const SESSION_MODE_CHAT: &str = "chat";
/// 首版唯一的模型来源：使用被控设备（B）本机模型。
const MODEL_SOURCE_CONTROLLED: &str = "controlled_device";
/// 一个连接上同时存在的会话意图上限（内存态，防无界增长）。
const MAX_SESSION_INTENTS: usize = 4;
const MAX_TITLE_CHARS: usize = 120;
/// 一个会话内跟踪的任务意图上限（超出后拒绝新提交，绝不静默丢弃）。
const MAX_TRACKED_TASKS: usize = 64;
const MAX_WORKSPACES: usize = 16;
const MAX_MODES: usize = 2;
const MAX_PENDING_CAPABILITY_REQUESTS: usize = 4;
/// 输入摘要的域分离前缀：同参必须同摘要、异参必须不同摘要。
const TASK_DIGEST_DOMAIN: &[u8] = b"pinvou-controller-task-input-v1";

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub(crate) struct SessionError {
    pub(crate) code: &'static str,
}

impl SessionError {
    const fn new(code: &'static str) -> Self {
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
    pub(crate) lease_epoch: String,
    pub(crate) remote_control_generation: String,
    pub(crate) capability_revision: Option<u64>,
    pub(crate) capability_valid_for_ms: Option<u64>,
    pub(crate) capability_error: Option<String>,
    pub(crate) capability_request_id: Option<String>,
    pub(crate) modes: Vec<String>,
    pub(crate) workspace_count: usize,
    pub(crate) task_count: usize,
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
            lease_epoch: self.lease.lease_epoch.clone(),
            remote_control_generation: self.lease.remote_control_generation.clone(),
            capability_revision: self.capability_revision,
            capability_valid_for_ms: self.capability_valid_for_ms,
            capability_error: self.capability_error.clone(),
            capability_request_id: self.last_capability_request_id.clone(),
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
    pub(crate) fn open(
        &self,
        remote_ref: Option<&str>,
        title: &str,
        mode: &str,
        lease: &LeaseContext,
        negotiated_capabilities: &[String],
    ) -> Result<ControllingSessionProjection> {
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
        if let Some(existing) = intents.iter_mut().find(|intent| intent.session_ref == session_ref) {
            ensure_context(existing, lease)?;
            return Ok(existing.projection());
        }
        if intents.len() >= MAX_SESSION_INTENTS {
            return Err(SessionError::new("session_capacity"));
        }
        let intent = SessionIntent {
            session_ref,
            session_id: format!("session-{}", Uuid::new_v4().simple()),
            title: title.to_owned(),
            mode: SESSION_MODE_CHAT.to_owned(),
            model_source: MODEL_SOURCE_CONTROLLED.to_owned(),
            lease: lease.clone(),
            negotiated_capabilities: negotiated_capabilities.to_vec(),
            capability_revision: None,
            capability_valid_for_ms: None,
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
        Ok(projection)
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

    /// 应用 B 的能力快照：校验冻结形状、lease 绑定与 revision 单调性，并记录
    /// 协商出的模式与工作区。revision 回退或形状不符都失败关闭。
    pub(crate) fn apply_capability_snapshot(
        &self,
        remote_ref: &str,
        payload: &Value,
        lease: &LeaseContext,
    ) -> Result<ControllingSessionProjection> {
        let mut intents = self.intents.lock();
        let intent = find_intent(&mut intents, remote_ref)?;
        ensure_context(intent, lease)?;
        apply_snapshot_locked(intent, payload)
    }

    /// 按 lease + request_id 精确定位请求会话并应用快照；若对端额外携带
    /// session_id，则同时校验两者，避免任何跨会话响应被接受。
    pub(crate) fn apply_capability_snapshot_for_lease(
        &self,
        lease: &LeaseContext,
        payload: &Value,
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
        let projection = apply_snapshot_locked(intent, payload)?;
        intent.pending_capability_requests.clear();
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
        let text = text.trim();
        if text.is_empty() || text.len() > MAX_TASK_INPUT_TEXT_BYTES {
            return Err(SessionError::new("task_input_invalid"));
        }
        let mut intents = self.intents.lock();
        let intent = find_intent(&mut intents, remote_ref)?;
        ensure_context(intent, lease)?;
        // 未取到能力快照前不构造任务：revision 是任务接纳的准入条件。
        if intent.capability_revision.is_none() {
            return Err(SessionError::new("capability_snapshot_required"));
        }
        if intent.tasks.len() >= MAX_TRACKED_TASKS {
            return Err(SessionError::new("task_capacity"));
        }
        let request_id = intent.next_request_id();
        let task_id = format!("task-{}", Uuid::new_v4().simple());
        let client_request_id = format!("client-request-{}", Uuid::new_v4().simple());
        let payload = build_task_submit(intent, &request_id, &task_id, &client_request_id, text)?;
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
) -> Result<ControllingSessionProjection> {
    let validated = validate_inbound_payload(intent, payload, "capability_snapshot")?;
    let revision = validated
        .get("capability_revision")
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<u64>().ok())
        .ok_or_else(|| SessionError::new("capability_snapshot_invalid"))?;
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
        if workspace_ids.len() > MAX_WORKSPACES {
            return Err(SessionError::new("capability_snapshot_invalid"));
        }
    }
    intent.capability_revision = Some(revision);
    intent.capability_valid_for_ms = validated
        .get("valid_for_ms")
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<u64>().ok())
        .or(Some(30_000));
    intent.capability_error = None;
    intent.last_capability_request_id = Some(response_request_id(&validated)?.to_owned());
    intent.modes = modes;
    intent.workspace_ids = workspace_ids;
    Ok(intent.projection())
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
) -> Result<Value> {
    let revision = intent
        .capability_revision
        .ok_or_else(|| SessionError::new("capability_snapshot_required"))?;
    let input = json!({
        "text": text,
        "attachment_ids": Vec::<String>::new(),
        "skill_ids": Vec::<String>::new(),
    });
    let payload = json!({
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
        "model_source": intent.model_source,
        "mode": intent.mode,
        "input_digest": task_input_digest(text),
        "input": input,
    });
    validate_outbound_payload(intent, &payload)
}

fn build_task_cancel(intent: &SessionIntent, request_id: &str, task_id: &str) -> Result<Value> {
    let payload = json!({
        "v": 1,
        "type": "task_cancel",
        "connection_id": intent.lease.connection_id,
        "controller_lease_id": intent.lease.controller_lease_id,
        "lease_epoch": intent.lease.lease_epoch,
        "relay_epoch": intent.lease.relay_epoch,
        "remote_control_generation": intent.lease.remote_control_generation,
        "request_id": request_id,
        "task_id": task_id,
        "session_id": intent.session_id,
    });
    validate_outbound_payload(intent, &payload)
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
    let encoded =
        serde_json::to_string(payload).map_err(|_| SessionError::new("capability_snapshot_invalid"))?;
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
            .open(None, "  Weekly report  ", "chat", &lease(), &capabilities())
            .unwrap();
        assert_eq!(projection.title, "Weekly report");
        assert_eq!(projection.mode, "chat");
        assert_eq!(projection.model_source, "controlled_device");
        assert_eq!(projection.connection_id, "connection-0001");
        assert_eq!(projection.lease_epoch, "1");
        assert!(projection.session_id.starts_with("session-"));
        assert!(is_valid_identifier(&projection.session_id));
        assert_eq!(projection.capability_revision, None);
        assert_eq!(projection.modes, vec!["chat".to_owned()]);

        // 首版能力开关：代码模式、空标题与非法引用都失败关闭。
        assert_eq!(
            sessions
                .open(None, "Code", "code", &lease(), &capabilities())
                .unwrap_err()
                .code,
            "session_mode_unavailable"
        );
        assert_eq!(
            sessions
                .open(None, "   ", "chat", &lease(), &capabilities())
                .unwrap_err()
                .code,
            "session_title_invalid"
        );
        assert_eq!(
            sessions
                .open(Some("bad ref"), "Title", "chat", &lease(), &capabilities())
                .unwrap_err()
                .code,
            "session_reference_invalid"
        );
        // 同名 ref 复用同一意图（幂等打开）。
        let reopened = sessions
            .open(
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
            .open(None, "Session", "chat", &lease(), &capabilities())
            .unwrap();
        let request = sessions
            .capability_request(&session.session_ref, &lease())
            .unwrap();
        assert_eq!(request.payload["type"], "capability_snapshot_request");
        assert_eq!(request.payload["known_revision"], "0");
        assert_eq!(request.payload["session_id"], session.session_id);

        let applied = sessions
            .apply_capability_snapshot(&session.session_ref, &snapshot(4), &lease())
            .unwrap();
        assert_eq!(applied.capability_revision, Some(4));
        assert_eq!(applied.workspace_count, 1);

        // 同 revision 幂等；回退失败关闭。
        let idempotent = sessions
            .apply_capability_snapshot(&session.session_ref, &snapshot(4), &lease())
            .unwrap();
        assert_eq!(idempotent.capability_revision, Some(4));
        assert_eq!(
            sessions
                .apply_capability_snapshot(&session.session_ref, &snapshot(3), &lease())
                .unwrap_err()
                .code,
            "capability_snapshot_stale"
        );
        // 跨 lease 的快照必须失败关闭（lease_epoch 变化的旧快照）。
        let mut foreign = snapshot(5);
        foreign["lease_epoch"] = json!("2");
        assert_eq!(
            sessions
                .apply_capability_snapshot(&session.session_ref, &foreign, &lease())
                .unwrap_err()
                .code,
            "capability_snapshot_invalid"
        );
        // 跨连接（A 端 lease 已变化）同样失败关闭。
        let mut other_lease = lease();
        other_lease.remote_control_generation = "generation-0002".into();
        assert_eq!(
            sessions
                .apply_capability_snapshot(&session.session_ref, &snapshot(6), &other_lease)
                .unwrap_err()
                .code,
            "session_context_stale"
        );
    }

    #[test]
    fn capability_responses_are_request_and_session_bound() {
        let sessions = ControllingSessionManager::default();
        let first = sessions
            .open(Some("session-ref-first"), "First", "chat", &lease(), &capabilities())
            .unwrap();
        let second = sessions
            .open(Some("session-ref-second"), "Second", "chat", &lease(), &capabilities())
            .unwrap();
        let request = sessions
            .capability_request(&second.session_ref, &lease())
            .unwrap();
        let mut response = snapshot(5);
        response["request_id"] = json!(request.request_id);

        let applied = sessions
            .apply_capability_snapshot_for_lease(&lease(), &response)
            .unwrap();
        assert_eq!(applied.session_ref, second.session_ref);
        assert_eq!(applied.capability_revision, Some(5));
        assert_eq!(
            sessions.state(&first.session_ref, &lease()).unwrap().capability_revision,
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
            "code": "capability_snapshot_unavailable",
        });
        let failed = sessions
            .apply_capability_error_for_lease(&lease(), &failure)
            .unwrap();
        assert_eq!(failed.session_ref, first.session_ref);
        assert_eq!(failed.capability_error.as_deref(), Some("capability_snapshot_unavailable"));
        assert_eq!(failed.capability_revision, None);
    }

    #[test]
    fn relay_failure_is_only_associated_when_one_capability_session_is_pending() {
        let sessions = ControllingSessionManager::default();
        let first = sessions
            .open(Some("session-ref-first"), "First", "chat", &lease(), &capabilities())
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
            .open(Some("session-ref-second"), "Second", "chat", &lease(), &capabilities())
            .unwrap();
        sessions
            .capability_request(&first.session_ref, &lease())
            .unwrap();
        sessions
            .capability_request(&second.session_ref, &lease())
            .unwrap();
        assert_eq!(
            sessions
                .fail_pending_capability_for_lease(
                    &lease(),
                    "capability_snapshot_unavailable",
                )
                .unwrap_err()
                .code,
            "capability_response_ambiguous"
        );
        assert_eq!(
            sessions.state(&first.session_ref, &lease()).unwrap().capability_error,
            None
        );
        assert_eq!(
            sessions.state(&second.session_ref, &lease()).unwrap().capability_error,
            None
        );
    }

    #[test]
    fn task_intent_builds_a_frozen_payload_and_requires_a_snapshot() {
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open(None, "Session", "chat", &lease(), &capabilities())
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
            .apply_capability_snapshot(&session.session_ref, &snapshot(4), &lease())
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
    fn capabilities_change_how_requests_fail_closed() {
        let sessions = ControllingSessionManager::default();
        // 未协商 task_v1 时，快照请求本身就非法（能力闭集失败关闭）。
        let without_task = vec!["encrypted_frames_v1".to_owned(), "pairing_v1".to_owned()];
        let session = sessions
            .open(None, "Session", "chat", &lease(), &without_task)
            .unwrap();
        assert_eq!(
            sessions
                .capability_request(&session.session_ref, &lease())
                .unwrap_err()
                .code,
            "capability_not_negotiated"
        );
    }

    #[test]
    fn closing_and_clearing_drop_intents() {
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open(None, "Session", "chat", &lease(), &capabilities())
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
                .open(
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
                .open(None, "Session", "chat", &lease(), &capabilities())
                .unwrap_err()
                .code,
            "session_capacity"
        );
        sessions.clear();
        assert_eq!(
            sessions
                .state("session-ref-0", &lease())
                .unwrap_err()
                .code,
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
