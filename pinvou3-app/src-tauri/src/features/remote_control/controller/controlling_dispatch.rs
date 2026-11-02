//! Single production inbound dispatcher, shared with the resource-bounded harness.
use super::super::controlling::{ControllerOperation, ControllingDeviceManager, ControllingStatus};
use super::super::controlling_session::{ControllingSessionManager, ControllingSessionProjection};
use super::super::encrypted_lane::ControllingLaneState;
use super::super::journal::{JournalEvent, JournalSnapshot, MAX_EVENT_PAYLOAD_BYTES};
use super::super::protocol::{
    LeaseContext, RuntimeValidationContext, SenderRole, validate_controller_runtime_message_json,
};
use super::super::relay_runtime::log_controller_event;
use super::lane_monotonic_millis;
use base64::Engine as _;
use serde_json::Value;
use std::time::Duration;
use uuid::Uuid;
/// A 端权威 journal 落库端口（E2E-05）：由命令层实现——按会话引用写入 A 的
/// SessionStore.RemoteProxy 投影正文；feature 层只负责解密与校验。
/// Constructed only inside the AEAD-opened business-frame dispatcher. The
/// app receives an authenticated frame, never a caller-asserted boolean.
pub(crate) struct AuthenticatedJournalFrame<'a> {
    operation: &'a ControllerOperation,
    lease: &'a LeaseContext,
    event: &'a JournalEvent,
}

impl AuthenticatedJournalFrame<'_> {
    pub(crate) fn operation(&self) -> &ControllerOperation {
        self.operation
    }
    pub(crate) fn lease(&self) -> &LeaseContext {
        self.lease
    }
    pub(crate) fn event(&self) -> &JournalEvent {
        self.event
    }
}

pub(crate) struct AuthenticatedSnapshotFrame<'a> {
    operation: &'a ControllerOperation,
    lease: &'a LeaseContext,
    snapshot: &'a JournalSnapshot,
}

impl AuthenticatedSnapshotFrame<'_> {
    pub(crate) fn operation(&self) -> &ControllerOperation {
        self.operation
    }
    pub(crate) fn lease(&self) -> &LeaseContext {
        self.lease
    }
    pub(crate) fn snapshot(&self) -> &JournalSnapshot {
        self.snapshot
    }
}

/// 通道保活期间的事件出口（状态/会话/任务），由 Tauri 层注入具体广播。
pub(crate) struct ControllingLaneSinks<'a> {
    pub(crate) on_status: &'a mut (dyn FnMut(&ControllingStatus) + Send),
    pub(crate) on_session: &'a mut (dyn FnMut(&ControllingSessionProjection) + Send),
    /// Authenticated capability response mapping, before A capability state advances.
    pub(crate) on_mapping:
        &'a mut (dyn FnMut(&LeaseContext, &str, &str) -> Result<(), String> + Send),
    pub(crate) on_task: &'a mut (dyn FnMut(&LeaseContext, Value) + Send),
    /// 解密并校验过的 B 权威 journal：A 端仅投影到 RemoteProxy。
    pub(crate) on_journal:
        &'a mut (dyn FnMut(&AuthenticatedJournalFrame<'_>) -> Result<(), String> + Send),
    /// 权威快照（E2E-05 缺口恢复）：A 端据此原子替换本地副本。
    pub(crate) on_snapshot:
        &'a mut (dyn FnMut(&AuthenticatedSnapshotFrame<'_>) -> Result<(), String> + Send),
    /// E2E-06：待发送的 A 端心跳帧（明文控制帧，绑定当前租约）。
    // True means authenticated transport admission succeeded, not remote receipt.
    pub(crate) on_liveness: &'a mut (dyn FnMut(Value) -> bool + Send),
}

/// 解密并派发一条入站业务帧。任何一层不符（非加密帧、无活租约、解密/校验
/// 失败）都静默丢弃——攻击者不能通过伪造帧影响任何状态。
pub(super) fn dispatch_inbound_frame(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    journal: &mut JournalExpectations,
    liveness: &mut LivenessExpectations,
    json: &str,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    let Ok(frame) = serde_json::from_str::<Value>(json) else {
        return;
    };
    let Ok((runtime, epoch_a)) = manager
        .with_peer_liveness_context(operation, |runtime, _, a| (runtime.clone(), a.to_owned()))
    else {
        return;
    };
    let Some(lease) = runtime.lease else {
        return;
    };
    let capabilities = runtime.negotiated_capabilities;
    if frame.get("type").and_then(Value::as_str) == Some("error") {
        if let Some(request_id) = frame.get("request_id").and_then(Value::as_str) {
            // A correlated negative response may advance retry to the next
            // heartbeat; it never confirms/ACKs a journal cursor.
            journal.expedite_negative_response(request_id, tokio::time::Instant::now());
        }
        let relay_code = frame
            .get("code")
            .and_then(Value::as_str)
            .unwrap_or("invalid_message");
        log_controller_event("runtime_relay_error", relay_code);
        // Encrypted business frames contain no request ID at the relay layer.
        // A plaintext relay error therefore cannot identify a pending capability
        // request, including after a same-lease session is closed and reopened.
        // The bridge retains its bounded response timeout and authenticated B
        // capability replies remain correlated by their encrypted request ID.
        return;
    }
    // Relay 按冻结协议把 B 的挑战作为顶层明文控制帧转发给 A。它不是业务
    // `encrypted_frame`，但必须先通过角色、租约和能力绑定校验，随后才生成由
    // A/B 会话密钥保护的直接存活证明。此前该分支位于 encrypted_frame 解密后，
    // 导致真实挑战在到达应答逻辑之前被静默丢弃。
    if frame.get("type").and_then(Value::as_str) == Some("controller_liveness_challenge_a") {
        dispatch_liveness_challenge(
            manager,
            operation,
            business,
            liveness,
            &frame,
            &lease,
            &capabilities,
            &epoch_a,
            sinks,
        );
        return;
    }
    if frame.get("type").and_then(Value::as_str) != Some("encrypted_frame") {
        return;
    }
    let Ok(Ok((payload_kind, payload))) =
        manager.with_lane_owner(operation, || business.open(&frame, &capabilities))
    else {
        return;
    };
    dispatch_authenticated_payload(
        manager,
        operation,
        sessions,
        business,
        journal,
        &lease,
        &capabilities,
        &payload_kind,
        payload,
        sinks,
    );
}

/// AEAD admission and projection are separate boundaries: final shared-state
/// commits still validate this original owner after a replacement can occur.
pub(super) fn dispatch_authenticated_payload(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    sessions: &ControllingSessionManager,
    business: &ControllingLaneState,
    journal: &mut JournalExpectations,
    lease: &LeaseContext,
    capabilities: &[String],
    payload_kind: &str,
    payload: Value,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    if manager.with_lane_owner(operation, || ()).is_err() {
        return;
    }
    match payload_kind {
        "capability_snapshot" => {
            match sessions.apply_capability_snapshot_for_lease_with_mapping_and_guard(
                &lease,
                &payload,
                |remote_ref, controlled_session_id| {
                    (sinks.on_mapping)(&lease, remote_ref, controlled_session_id).map_err(|_| {
                        super::super::controlling_session::SessionError::new(
                            "remote_mapping_unavailable",
                        )
                    })
                },
                |commit| {
                    manager.with_lane_owner(operation, commit).map_err(|_| {
                        super::super::controlling_session::SessionError::new(
                            "session_context_stale",
                        )
                    })
                },
            ) {
                Ok(projection) => {
                    log_controller_event("capability_snapshot_applied", "ready");
                    (sinks.on_session)(&projection);
                }
                Err(error) => {
                    log_controller_event("capability_snapshot_rejected", error.code);
                }
            }
        }
        "capability_snapshot_error" => {
            match sessions.apply_capability_error_for_lease_with_guard(&lease, &payload, |commit| {
                manager.with_lane_owner(operation, commit).map_err(|_| {
                    super::super::controlling_session::SessionError::new("session_context_stale")
                })?
            }) {
                Ok(projection) => {
                    log_controller_event("capability_snapshot_failed", "unavailable");
                    (sinks.on_session)(&projection);
                }
                Err(error) => {
                    log_controller_event("capability_error_rejected", error.code);
                }
            }
        }
        "task_status" => {
            if let Some(event) = task_event_projection(&lease, &payload) {
                log_controller_event(
                    "task_status_received",
                    event
                        .get("status")
                        .and_then(Value::as_str)
                        .unwrap_or("unknown"),
                );
                (sinks.on_task)(&lease, event);
            }
        }
        "remote_action_result" => {
            // Only the authenticated decrypt path may reach this callback.
            // The adapter never emits the raw result to WebView.
            (sinks.on_task)(&lease, payload);
        }
        "journal_event" => {
            let Some(event) = journal_event_from_wire(&payload) else {
                log_controller_event("journal_event_rejected", "invalid_envelope_or_identity");
                return;
            };
            if journal.is_retired(&event.session_id, &event.epoch) {
                log_controller_event("journal_event_ignored", "retired_epoch");
                return;
            }
            match project_journal_event(journal, &event, &mut |event| {
                (sinks.on_journal)(&AuthenticatedJournalFrame {
                    operation,
                    lease: &lease,
                    event,
                })
            }) {
                Ok(JournalAdmission::Apply) => {
                    log_controller_event("journal_event_applied", &event.kind);
                }
                Ok(JournalAdmission::Duplicate) => {
                    log_controller_event("journal_event_verified", "duplicate");
                }
                Err(_) => log_controller_event("journal_event_rejected", "projection_failed"),
                Ok(JournalAdmission::RequestReplay { epoch, after }) => {
                    log_controller_event("journal_event_gap", "replay_requested");
                    begin_and_send_replay(
                        journal,
                        &event.session_id,
                        &epoch,
                        after,
                        event.sequence,
                        &lease.remote_control_generation,
                        tokio::time::Instant::now(),
                        |pending| {
                            request_journal_replay(
                                manager,
                                operation,
                                business,
                                &lease,
                                &capabilities,
                                &event.session_id,
                                &pending.epoch,
                                pending.after,
                                &pending.request_id,
                            )
                        },
                    );
                }
            }
        }
        "session_snapshot" => {
            let Some(snapshot) = journal_snapshot_from_wire(&payload) else {
                return;
            };
            // Equal-base snapshots still enter the authoritative sink so
            // DEV-30 can verify immutable content; transport cache is not ACK
            // authority and may never skip that comparison.
            match project_journal_snapshot(journal, &snapshot, &mut |snapshot| {
                (sinks.on_snapshot)(&AuthenticatedSnapshotFrame {
                    operation,
                    lease: &lease,
                    snapshot,
                })
            }) {
                Ok(()) => {}
                Err(_) => log_controller_event("journal_snapshot_rejected", "projection_failed"),
            }
        }
        _ => {}
    }
}

/// 严格校验并应答 B 的顶层存活挑战。Relay 只能转发挑战，不能构造这里返回的
/// AEAD 证明；因此明文控制帧仍不会被直接当成 B 的有效存活证据。
pub(super) fn dispatch_liveness_challenge(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    business: &ControllingLaneState,
    liveness: &mut LivenessExpectations,
    frame: &Value,
    lease: &LeaseContext,
    capabilities: &[String],
    epoch: &str,
    sinks: &mut ControllingLaneSinks<'_>,
) {
    let context = RuntimeValidationContext {
        lease: Some(lease.clone()),
        negotiated_capabilities: capabilities.to_vec(),
    };
    let Ok(challenge) = validate_controller_runtime_message_json(
        &frame.to_string(),
        SenderRole::ControlledB,
        Some(&context),
    ) else {
        return;
    };
    let Some(challenge_id) = challenge.get("challenge_id").and_then(Value::as_str) else {
        return;
    };
    let Some(challenge_b64) = challenge.get("challenge_b64").and_then(Value::as_str) else {
        return;
    };
    let now_a = lane_monotonic_millis();
    if !liveness.can_answer(challenge_id, now_a) {
        return;
    }
    let Some(sequence) = liveness.next_sequence() else {
        return;
    };
    let inner = serde_json::json!({
        "v": 1,
        "type": "controller_liveness_a",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "role": "A",
        "liveness_epoch_a": epoch,
        "heartbeat_sequence_a": sequence.to_string(),
        "challenge_id": challenge_id,
        "challenge_b64": challenge_b64,
    });
    let Ok(Ok(response)) =
        manager.with_lane_owner(operation, || business.seal_liveness(&inner, capabilities))
    else {
        return;
    };
    if (sinks.on_liveness)(response) {
        liveness.record_answered(challenge_id, now_a);
    }
}

/// wire `session_snapshot` → 权威快照。base64 体积与声明一致、摘要与快照本体
/// 自带的 digest 一致才接受；事件条目沿用与增量事件相同的体积上限。
pub(super) fn journal_snapshot_from_wire(payload: &Value) -> Option<JournalSnapshot> {
    let encoded = payload.get("snapshot_b64").and_then(Value::as_str)?;
    let declared = payload
        .get("snapshot_bytes")
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<usize>().ok())?;
    let raw = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .ok()?;
    if raw.len() != declared || raw.len() > MAX_SNAPSHOT_BYTES {
        return None;
    }
    let snapshot: JournalSnapshot = serde_json::from_slice(&raw).ok()?;
    if !snapshot.verify_digest()
        || payload.get("session_id").and_then(Value::as_str) != Some(snapshot.session_id.as_str())
        || payload.get("sha256").and_then(Value::as_str) != Some(snapshot.digest.as_str())
        || payload.get("journal_epoch").and_then(Value::as_str) != Some(snapshot.epoch.as_str())
        || payload.get("base_sequence").and_then(Value::as_str)
            != Some(snapshot.sequence.to_string().as_str())
    {
        return None;
    }
    Some(snapshot)
}

/// 快照帧的硬上限：256 条事件 × 单条上限 + 信封开销。
pub(super) const MAX_SNAPSHOT_BYTES: usize = 256 * MAX_EVENT_PAYLOAD_BYTES;

/// wire `journal_event` → 权威事件。`event_b64` 是事件本体（冻结的 camelCase
/// 形状），明文侧还有 journal_epoch/event_sequence/event_kind 供缺口检测；三者
/// 必须互相一致，否则丢弃。事件体积有硬上限。明文 `event_kind` 是冻结闭集
/// （如 `terminal`），事件本体保留更细的内部类型，因此只核对 epoch/序号/会话。
pub(super) fn journal_event_from_wire(payload: &Value) -> Option<JournalEvent> {
    let encoded = payload.get("event_b64").and_then(Value::as_str)?;
    let declared = payload
        .get("event_bytes")
        .and_then(Value::as_str)
        .and_then(|value| value.parse::<usize>().ok())?;
    let raw = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .ok()?;
    if raw.len() != declared || raw.len() > MAX_EVENT_PAYLOAD_BYTES {
        return None;
    }
    let event: JournalEvent = serde_json::from_slice(&raw).ok()?;
    if event.validate_exportable().is_err()
        || payload.get("event_kind").and_then(Value::as_str) != event.wire_kind()
        || payload.get("journal_epoch").and_then(Value::as_str) != Some(event.epoch.as_str())
        || payload.get("event_sequence").and_then(Value::as_str)
            != Some(event.sequence.to_string().as_str())
        || payload.get("session_id").and_then(Value::as_str) != Some(event.session_id.as_str())
    {
        return None;
    }
    Some(event)
}

/// A 端 transport 顺序提示；重复仍须交给权威投影端口验证内容等价。
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) enum JournalAdmission {
    Apply,
    RequestReplay { epoch: String, after: u64 },
    Duplicate,
}

/// 纯函数：依据本端已知的 (epoch, last_sequence) 与到达事件推导接纳动作。
/// 换 epoch 时从该 epoch 的 0 号位请求回放（B 侧保留窗口不足会给快照）。
pub(crate) fn journal_admission(
    expected: Option<(&str, u64)>,
    event: &JournalEvent,
) -> JournalAdmission {
    match expected {
        None if event.sequence == 1 => JournalAdmission::Apply,
        None => JournalAdmission::RequestReplay {
            epoch: event.epoch.clone(),
            after: 0,
        },
        Some((epoch, _)) if epoch != event.epoch => JournalAdmission::RequestReplay {
            epoch: event.epoch.clone(),
            after: 0,
        },
        Some((_, last)) if event.sequence <= last => JournalAdmission::Duplicate,
        Some((_, last)) if event.sequence == last.saturating_add(1) => JournalAdmission::Apply,
        Some((_, last)) => JournalAdmission::RequestReplay {
            epoch: event.epoch.clone(),
            after: last,
        },
    }
}

pub(super) fn project_journal_event(
    cache: &mut JournalExpectations,
    event: &JournalEvent,
    sink: &mut dyn FnMut(&JournalEvent) -> Result<(), String>,
) -> Result<JournalAdmission, String> {
    let admission = journal_admission(cache.expected(&event.session_id), event);
    if matches!(
        admission,
        JournalAdmission::Apply | JournalAdmission::Duplicate
    ) {
        sink(event)?;
        if admission == JournalAdmission::Apply {
            cache.mark_applied(event);
        }
    }
    Ok(admission)
}

pub(super) fn project_journal_snapshot(
    cache: &mut JournalExpectations,
    snapshot: &JournalSnapshot,
    sink: &mut dyn FnMut(&JournalSnapshot) -> Result<(), String>,
) -> Result<(), String> {
    if cache.is_retired(&snapshot.session_id, &snapshot.epoch) {
        return Ok(());
    }
    let same_or_older = cache
        .expected(&snapshot.session_id)
        .filter(|(epoch, _)| *epoch == snapshot.epoch)
        .map(|(_, sequence)| sequence.cmp(&snapshot.sequence));
    if same_or_older == Some(std::cmp::Ordering::Greater) {
        return Ok(());
    }
    // An equal-base snapshot may carry conflicting content. The durable
    // projection port must decide; this cache cannot pronounce it duplicate.
    sink(snapshot)?;
    if same_or_older != Some(std::cmp::Ordering::Equal) {
        cache.mark_snapshot(snapshot);
    }
    Ok(())
}

/// A 端存活状态：与心跳共享的单调序号 + 已应答过的挑战（同一挑战只答一次，
/// 避免重复投递导致重复加密大帧）。
#[derive(Default)]
pub(crate) struct LivenessExpectations {
    sequence: u64,
    pub(super) answered: std::collections::BTreeMap<String, u64>,
}

pub(super) const MAX_ANSWERED_LIVENESS_CHALLENGES: usize = 64;

impl LivenessExpectations {
    pub(crate) fn next_sequence(&mut self) -> Option<u64> {
        self.sequence = self.sequence.checked_add(1)?;
        Some(self.sequence)
    }

    pub(super) fn can_answer(&mut self, challenge_id: &str, now_a: u64) -> bool {
        self.answered.retain(|_, answered_at| {
            now_a.saturating_sub(*answered_at) <= super::super::liveness::LIVENESS_TOTAL_BUDGET_MS
        });
        !self.answered.contains_key(challenge_id)
            && self.answered.len() < MAX_ANSWERED_LIVENESS_CHALLENGES
    }

    /// Seal/enqueue failures leave the challenge retryable. At capacity we
    /// fail closed instead of evicting replay history inside its validity window.
    pub(super) fn record_answered(&mut self, challenge_id: &str, now_a: u64) {
        self.answered.insert(challenge_id.to_owned(), now_a);
    }
}

/// Conservative transport replay cache, never projection/ACK authority.
/// A restart may lose these hints; only the DEV-30 durable projection decides
/// duplicate content and confirmed cursor. In-flight replay suppresses only
/// redundant transport requests, not calls into the projection sink.
/// The authenticated journal sink sends ACK only after DEV-30's RemoteProxy
/// projection confirms the durable cursor. This transport cache does not
/// grant ACK authority or introduce another protocol message type.
pub(super) const MAX_PENDING_REPLAYS: usize = 256;
pub(super) const REPLAY_RETRY_INTERVAL: Duration = Duration::from_secs(3);

#[derive(Clone, Debug)]
pub(crate) struct PendingReplay {
    pub(super) request_id: String,
    pub(super) epoch: String,
    pub(super) after: u64,
    pub(super) target_sequence: u64,
    pub(super) generation: String,
    pub(super) next_retry_at: tokio::time::Instant,
}

#[derive(Default)]
pub(crate) struct JournalExpectations {
    applied: std::collections::BTreeMap<String, (String, u64)>,
    pub(super) awaiting_replay: std::collections::BTreeMap<String, PendingReplay>,
    retired_epochs: std::collections::BTreeMap<String, std::collections::BTreeSet<String>>,
}

impl JournalExpectations {
    pub(crate) fn expected(&self, session_id: &str) -> Option<(&str, u64)> {
        self.applied
            .get(session_id)
            .map(|(epoch, sequence)| (epoch.as_str(), *sequence))
    }

    pub(crate) fn mark_applied(&mut self, event: &JournalEvent) {
        if let Some((old_epoch, _)) = self.applied.get(&event.session_id) {
            if old_epoch != &event.epoch {
                self.retired_epochs
                    .entry(event.session_id.clone())
                    .or_default()
                    .insert(old_epoch.clone());
            }
        }
        self.applied.insert(
            event.session_id.clone(),
            (event.epoch.clone(), event.sequence),
        );
        if self
            .awaiting_replay
            .get(&event.session_id)
            .is_some_and(|pending| {
                pending.epoch == event.epoch && event.sequence >= pending.target_sequence
            })
        {
            self.awaiting_replay.remove(&event.session_id);
        }
    }

    /// 快照基准对空事件窗口同样有效，归属使用快照的明确 session_id。
    pub(crate) fn mark_snapshot(&mut self, snapshot: &JournalSnapshot) {
        if let Some((old_epoch, _)) = self.applied.get(&snapshot.session_id) {
            if old_epoch != &snapshot.epoch {
                self.retired_epochs
                    .entry(snapshot.session_id.clone())
                    .or_default()
                    .insert(old_epoch.clone());
            }
        }
        self.applied.insert(
            snapshot.session_id.clone(),
            (snapshot.epoch.clone(), snapshot.sequence),
        );
        if self
            .awaiting_replay
            .get(&snapshot.session_id)
            .is_some_and(|pending| {
                pending.epoch == snapshot.epoch && snapshot.sequence >= pending.target_sequence
            })
        {
            self.awaiting_replay.remove(&snapshot.session_id);
        }
    }

    pub(crate) fn is_retired(&self, session_id: &str, epoch: &str) -> bool {
        self.retired_epochs
            .get(session_id)
            .is_some_and(|epochs| epochs.contains(epoch))
    }

    /// One stable request per session/epoch gap. The bounded entry survives
    /// both successful queueing without response and local seal/send failure;
    /// heartbeat retries it with the exact same request_id.
    pub(crate) fn begin_replay(
        &mut self,
        session_id: &str,
        epoch: &str,
        after: u64,
        target_sequence: u64,
        generation: &str,
        now: tokio::time::Instant,
    ) -> Option<PendingReplay> {
        if let Some(pending) = self.awaiting_replay.get_mut(session_id) {
            if pending.epoch == epoch && pending.generation == generation {
                pending.target_sequence = pending.target_sequence.max(target_sequence);
                return None;
            }
        }
        if !self.awaiting_replay.contains_key(session_id)
            && self.awaiting_replay.len() >= MAX_PENDING_REPLAYS
        {
            return None;
        }
        let pending = PendingReplay {
            request_id: format!("request-replay-{}", Uuid::new_v4().simple()),
            epoch: epoch.to_owned(),
            after,
            target_sequence,
            generation: generation.to_owned(),
            next_retry_at: now + REPLAY_RETRY_INTERVAL,
        };
        self.awaiting_replay
            .insert(session_id.to_owned(), pending.clone());
        Some(pending)
    }

    pub(crate) fn due_replays(
        &mut self,
        generation: &str,
        now: tokio::time::Instant,
    ) -> Vec<(String, PendingReplay)> {
        self.awaiting_replay
            .retain(|_, pending| pending.generation == generation);
        let mut due = Vec::new();
        for (session_ref, pending) in &mut self.awaiting_replay {
            if pending.next_retry_at <= now {
                pending.next_retry_at = now + REPLAY_RETRY_INTERVAL;
                due.push((session_ref.clone(), pending.clone()));
            }
        }
        due
    }

    pub(crate) fn expedite_negative_response(
        &mut self,
        request_id: &str,
        now: tokio::time::Instant,
    ) {
        if let Some(pending) = self
            .awaiting_replay
            .values_mut()
            .find(|pending| pending.request_id == request_id)
        {
            pending.next_retry_at = now;
        }
    }

    pub(crate) fn rearm_pending(&mut self, now: tokio::time::Instant) {
        for pending in self.awaiting_replay.values_mut() {
            pending.next_retry_at = now;
        }
    }
}

/// 冻结形状的 `journal_replay_request`（A→B 加密业务帧）。
pub(crate) fn build_journal_replay_request(
    lease: &LeaseContext,
    session_ref: &str,
    journal_epoch: &str,
    after_sequence: u64,
    request_id: &str,
) -> Result<Value, &'static str> {
    if session_ref.is_empty()
        || journal_epoch.is_empty()
        || request_id.is_empty()
        || request_id.len() > 128
    {
        return Err("journal_request_invalid");
    }
    Ok(serde_json::json!({
        "v": 1,
        "type": "journal_replay_request",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": request_id,
        "session_id": session_ref,
        "journal_epoch": journal_epoch,
        "after_sequence": after_sequence.to_string(),
    }))
}

/// 请求缺失区间的回放；失败不影响缺口事件的丢弃语义（下次事件仍会重试）。
pub(super) fn request_journal_replay(
    manager: &ControllingDeviceManager,
    operation: &ControllerOperation,
    business: &ControllingLaneState,
    lease: &LeaseContext,
    capabilities: &[String],
    session_ref: &str,
    journal_epoch: &str,
    after_sequence: u64,
    request_id: &str,
) -> bool {
    let Ok(payload) = build_journal_replay_request(
        lease,
        session_ref,
        journal_epoch,
        after_sequence,
        request_id,
    ) else {
        return false;
    };
    let Ok(sender) = manager.transport_for_operation(operation) else {
        return false;
    };
    manager
        .with_lane_owner(operation, || {
            let Ok(frame) = business.seal("journal_replay_request", &payload, capabilities) else {
                return false;
            };
            sender
                .as_ref()
                .is_some_and(|sender| sender.try_send_business(&frame).is_ok())
        })
        .unwrap_or(false)
}

pub(super) fn begin_and_send_replay(
    cache: &mut JournalExpectations,
    session_id: &str,
    epoch: &str,
    after: u64,
    target_sequence: u64,
    generation: &str,
    now: tokio::time::Instant,
    send: impl FnOnce(&PendingReplay) -> bool,
) -> bool {
    let Some(pending) =
        cache.begin_replay(session_id, epoch, after, target_sequence, generation, now)
    else {
        return false;
    };
    // A queued request is not a response. Keep the entry even if the local
    // seal/send fails, so a timer drives the next attempt without new events.
    send(&pending)
}

/// 任务状态投影（UI 任务域字段）；wire 的 `task_status` 只有状态与可选结果摘要。
pub(super) fn task_event_projection(lease: &LeaseContext, payload: &Value) -> Option<Value> {
    let task_id = payload.get("task_id").and_then(Value::as_str)?;
    let status = payload.get("task_status").and_then(Value::as_str)?;
    Some(serde_json::json!({
        "taskId": task_id,
        "status": status,
        "sessionId": payload.get("session_id").and_then(Value::as_str),
        "connectionId": lease.connection_id,
        "leaseEpoch": lease.lease_epoch,
        "remoteControlGeneration": lease.remote_control_generation,
        "resultDigest": payload.get("result_digest").and_then(Value::as_str),
        "rejectionCode": payload.get("rejection_code").and_then(Value::as_str),
        "clientRequestId": payload.get("client_request_id").and_then(Value::as_str),
        "outcomeUnknown": status == "result_unknown",
    }))
}

#[cfg(test)]
mod tests {
    use super::super::super::controlling::ControllerLease;
    use super::super::super::crypto::ControllerRole as HandshakeRole;
    use super::super::super::encrypted_lane::EncryptedLane;
    use super::super::super::relay_transport::{
        AuthenticatedLease, ControllerRole, transport_channels,
    };
    use super::super::super::test_support::{lease_context, paired_session_keys, pairing_fixture};
    use super::*;
    use serde_json::json;
    use std::cell::RefCell;
    const RELAY_KEY_ID: &str = "relay-key-service";
    const DEVICE_DISPLAY_NAME_PLACEHOLDER: &str = "remote-device";
    fn record_timeout_scenario(old_before_retry: bool) -> Value {
        let fixture = pairing_fixture(RELAY_KEY_ID);
        let lease = lease_context(&fixture.reservation);
        let (a_keys, b_keys) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let capabilities = vec!["encrypted_frames_v1".to_owned(), "task_v1".to_owned()];
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation.clone(),
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7; 32],
                    transport_key_fingerprint: [9; 32],
                    negotiated_capabilities: capabilities.clone(),
                },
            )
            .unwrap();
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControllerA,
            lease.remote_control_generation.clone(),
            1,
            1,
            8192,
            1024,
        );
        sender
            .install_authenticated_lease(AuthenticatedLease {
                context: lease.clone(),
                negotiated_capabilities: capabilities.clone(),
            })
            .unwrap();
        manager.replace_transport(sender);
        let business = ControllingLaneState::default();
        business.install(HandshakeRole::A, &lease, a_keys).unwrap();
        let mut responder = EncryptedLane::new(HandshakeRole::B, &lease, b_keys).unwrap();
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .unwrap()
            .0;
        let expected = manager.authenticated_remote_proxy_identity().unwrap();
        let send = |payload: &Value| {
            manager.send_capability_for_owner(&operation, &expected, &business, payload, |_| {
                "controller_lane_unavailable".to_owned()
            })
        };
        let old = sessions
            .capability_request_with_send(&session.session_ref, &lease, send)
            .unwrap();
        let old_frame: Value =
            serde_json::from_str(&receiver.take_business_for_test().unwrap()).unwrap();
        let (kind, old_payload) = responder.open(&old_frame, &capabilities).unwrap();
        assert_eq!(kind, "capability_snapshot_request");
        let snapshot = |request: &Value, revision: &str| {
            json!({
                "v":1, "type":"capability_snapshot", "connection_id":lease.connection_id,
                "controller_lease_id":lease.controller_lease_id, "lease_epoch":lease.lease_epoch,
                "relay_epoch":lease.relay_epoch, "remote_control_generation":lease.remote_control_generation,
                "request_id":request["request_id"], "session_id":request["session_id"],
                "controlled_session_id":"b-session-1", "capability_revision":revision,
                "expires_at_ms":"30000", "modes":["chat"], "tools":[], "skills":[], "workspaces":[]
            })
        };
        let old_reply = responder
            .seal(
                "capability_snapshot",
                &snapshot(&old_payload, "4"),
                &capabilities,
            )
            .unwrap();
        let projections = std::sync::Mutex::new(Vec::new());
        let mappings = std::sync::Mutex::new(Vec::<(String, String)>::new());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut |_| {},
            on_session: &mut |p| projections.lock().unwrap().push(p.clone()),
            on_mapping: &mut |_, r, id| {
                mappings.lock().unwrap().push((r.to_owned(), id.to_owned()));
                Ok(())
            },
            on_task: &mut |_, _| {},
            on_journal: &mut |_| Ok(()),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut |_| false,
        };
        let mut journal = JournalExpectations::default();
        let mut liveness = LivenessExpectations::default();
        // Relay error is plaintext and has no capability request identity.
        dispatch_inbound_frame(
            &manager,
            &operation,
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &json!({"type":"error", "code":"invalid_message"}).to_string(),
            &mut sinks,
        );
        assert!(projections.lock().unwrap().is_empty());
        assert!(mappings.lock().unwrap().is_empty());
        assert_eq!(
            sessions
                .pending_capability_ids_for_test(&session.session_ref)
                .unwrap(),
            vec![old.request_id.clone()]
        );
        if old_before_retry {
            dispatch_inbound_frame(
                &manager,
                &operation,
                &sessions,
                &business,
                &mut journal,
                &mut liveness,
                &old_reply.to_string(),
                &mut sinks,
            );
            assert_eq!(projections.lock().unwrap().len(), 1);
            assert_eq!(
                projections.lock().unwrap()[0]
                    .capability_request_id
                    .as_deref(),
                Some(old.request_id.as_str())
            );
        }
        let new = sessions
            .capability_request_with_send(&session.session_ref, &lease, send)
            .unwrap();
        let new_frame: Value =
            serde_json::from_str(&receiver.take_business_for_test().unwrap()).unwrap();
        let (kind, new_payload) = responder.open(&new_frame, &capabilities).unwrap();
        assert_eq!(kind, "capability_snapshot_request");
        assert_ne!(old.request_id, new.request_id);
        assert_eq!(old_payload["request_id"], old.request_id);
        assert_eq!(new_payload["request_id"], new.request_id);
        if !old_before_retry {
            assert_eq!(
                sessions
                    .pending_capability_ids_for_test(&session.session_ref)
                    .unwrap(),
                vec![old.request_id.clone(), new.request_id.clone()]
            );
            dispatch_inbound_frame(
                &manager,
                &operation,
                &sessions,
                &business,
                &mut journal,
                &mut liveness,
                &old_reply.to_string(),
                &mut sinks,
            );
            assert!(projections.lock().unwrap().is_empty());
            assert!(mappings.lock().unwrap().is_empty());
            assert_eq!(
                sessions
                    .pending_capability_ids_for_test(&session.session_ref)
                    .unwrap(),
                vec![old.request_id.clone(), new.request_id.clone()]
            );
        }
        assert!(matches!(
            manager
                .with_lane_owner(&operation, || business.open(&old_reply, &capabilities))
                .unwrap(),
            Err(super::super::super::encrypted_lane::LaneFailure::Replay)
        ));
        let old_event = projections.lock().unwrap().first().cloned();
        let new_reply = responder
            .seal(
                "capability_snapshot",
                &snapshot(&new_payload, "5"),
                &capabilities,
            )
            .unwrap();
        dispatch_inbound_frame(
            &manager,
            &operation,
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &new_reply.to_string(),
            &mut sinks,
        );
        let all = projections.lock().unwrap();
        let latest = all.last().unwrap();
        assert_eq!(
            latest.capability_request_id.as_deref(),
            Some(new.request_id.as_str())
        );
        assert_eq!(latest.capability_revision, Some(5));
        assert_eq!(all.len(), if old_before_retry { 2 } else { 1 });
        assert_eq!(
            mappings.lock().unwrap().len(),
            if old_before_retry { 2 } else { 1 }
        );
        assert!(
            sessions
                .pending_capability_ids_for_test(&session.session_ref)
                .unwrap()
                .is_empty()
        );
        json!({"oldAck":{"requestId":old.request_id,"sessionId":old.session_id},
               "newAck":{"requestId":new.request_id,"sessionId":new.session_id},
               "oldProjection":old_event, "newProjection":latest,
               "relayErrorProducedCapabilityEvent":false})
    }

    #[test]
    fn capture_timeout_dispatch_correlations() {
        let record = json!({"oldBeforeRetry":record_timeout_scenario(true), "oldAfterRetry":record_timeout_scenario(false)});
        if let Ok(path) = std::env::var("PINVOU_DISPATCH_RECORD") {
            std::fs::write(path, serde_json::to_vec_pretty(&record).unwrap()).unwrap();
        }
    }

    #[test]
    fn closed_reopened_same_reference_rejects_old_authenticated_reply() {
        let fixture = pairing_fixture(RELAY_KEY_ID);
        let lease = lease_context(&fixture.reservation);
        let (a_keys, b_keys) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let capabilities = vec!["encrypted_frames_v1".to_owned(), "task_v1".to_owned()];
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation.clone(),
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7; 32],
                    transport_key_fingerprint: [9; 32],
                    negotiated_capabilities: capabilities.clone(),
                },
            )
            .unwrap();
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControllerA,
            lease.remote_control_generation.clone(),
            1,
            1,
            8192,
            1024,
        );
        sender
            .install_authenticated_lease(AuthenticatedLease {
                context: lease.clone(),
                negotiated_capabilities: capabilities.clone(),
            })
            .unwrap();
        manager.replace_transport(sender);
        let business = ControllingLaneState::default();
        business.install(HandshakeRole::A, &lease, a_keys).unwrap();
        let mut responder = EncryptedLane::new(HandshakeRole::B, &lease, b_keys).unwrap();
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .unwrap()
            .0;
        let expected = manager.authenticated_remote_proxy_identity().unwrap();
        let send = |payload: &Value| {
            manager.send_capability_for_owner(&operation, &expected, &business, payload, |_| {
                "controller_lane_unavailable".to_owned()
            })
        };
        let old = sessions
            .capability_request_with_send(&session.session_ref, &lease, send)
            .unwrap();
        let old_frame: Value =
            serde_json::from_str(&receiver.take_business_for_test().unwrap()).unwrap();
        let (kind, old_payload) = responder.open(&old_frame, &capabilities).unwrap();
        assert_eq!(kind, "capability_snapshot_request");
        assert_eq!(old_payload["request_id"], old.request_id);
        assert_eq!(old_payload["session_id"], session.session_id);
        let snapshot = |request: &Value, revision: &str| {
            json!({
                "v": 1, "type": "capability_snapshot", "connection_id": lease.connection_id,
                "controller_lease_id": lease.controller_lease_id, "lease_epoch": lease.lease_epoch,
                "relay_epoch": lease.relay_epoch, "remote_control_generation": lease.remote_control_generation,
                "request_id": request["request_id"], "session_id": request["session_id"],
                "controlled_session_id": "b-session-1", "capability_revision": revision,
                "expires_at_ms": "30000", "modes": ["chat"], "tools": [], "skills": [], "workspaces": []
            })
        };
        // This valid B ciphertext exists before the old intent is closed.
        let old_reply = responder
            .seal(
                "capability_snapshot",
                &snapshot(&old_payload, "4"),
                &capabilities,
            )
            .unwrap();
        sessions.close(&session.session_ref).unwrap();
        let (reopened, created) = sessions
            .open_with_outcome(
                Some(&session.session_ref),
                "Session",
                "chat",
                &lease,
                &capabilities,
            )
            .unwrap();
        assert!(created);
        assert_eq!(reopened.session_ref, session.session_ref);
        assert_eq!(reopened.session_id, session.session_id);
        let new = sessions
            .capability_request_with_send(&reopened.session_ref, &lease, send)
            .unwrap();
        assert_ne!(new.request_id, old.request_id);
        let new_frame: Value =
            serde_json::from_str(&receiver.take_business_for_test().unwrap()).unwrap();
        let (kind, new_payload) = responder.open(&new_frame, &capabilities).unwrap();
        assert_eq!(kind, "capability_snapshot_request");
        assert_eq!(new_payload["request_id"], new.request_id);
        assert_eq!(new_payload["session_id"], old_payload["session_id"]);
        assert_eq!(
            sessions
                .pending_capability_ids_for_test(&session.session_ref)
                .unwrap(),
            vec![new.request_id.clone()]
        );
        assert_eq!(
            sessions
                .state(&session.session_ref, &lease)
                .unwrap()
                .capability_revision,
            None
        );
        let projections = std::sync::Mutex::new(Vec::new());
        let mappings = std::sync::Mutex::new(Vec::new());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut |_| {},
            on_session: &mut |projection| projections.lock().unwrap().push(projection.clone()),
            on_mapping: &mut |_, remote_ref, controlled_id| {
                mappings
                    .lock()
                    .unwrap()
                    .push((remote_ref.to_owned(), controlled_id.to_owned()));
                Ok(())
            },
            on_task: &mut |_, _| {},
            on_journal: &mut |_| Ok(()),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut |_| false,
        };
        let mut journal = JournalExpectations::default();
        let mut liveness = LivenessExpectations::default();
        dispatch_inbound_frame(
            &manager,
            &operation,
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &old_reply.to_string(),
            &mut sinks,
        );
        // A replay rejection proves the dispatcher opened the old ciphertext, not merely dropped it before AEAD.
        assert!(matches!(
            manager
                .with_lane_owner(&operation, || business.open(&old_reply, &capabilities))
                .unwrap(),
            Err(super::super::super::encrypted_lane::LaneFailure::Replay)
        ));
        // Barrier: stale success neither maps, publishes, changes revision, nor consumes new request.
        assert!(projections.lock().unwrap().is_empty());
        assert!(mappings.lock().unwrap().is_empty());
        assert_eq!(
            sessions
                .state(&session.session_ref, &lease)
                .unwrap()
                .capability_revision,
            None
        );
        assert_eq!(
            sessions
                .pending_capability_ids_for_test(&session.session_ref)
                .unwrap(),
            vec![new.request_id.clone()]
        );
        // Positive control, same owner/lease/session and live cipher: the new response is accepted.
        let new_reply = responder
            .seal(
                "capability_snapshot",
                &snapshot(&new_payload, "5"),
                &capabilities,
            )
            .unwrap();
        dispatch_inbound_frame(
            &manager,
            &operation,
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &new_reply.to_string(),
            &mut sinks,
        );
        assert_eq!(
            mappings.lock().unwrap().as_slice(),
            &[(session.session_ref.clone(), "b-session-1".to_owned())]
        );
        assert_eq!(projections.lock().unwrap().len(), 1);
        assert_eq!(
            projections.lock().unwrap()[0]
                .capability_request_id
                .as_deref(),
            Some(new.request_id.as_str())
        );
        assert_eq!(
            sessions
                .state(&session.session_ref, &lease)
                .unwrap()
                .capability_revision,
            Some(5)
        );
        assert!(
            sessions
                .pending_capability_ids_for_test(&session.session_ref)
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn capability_queue_failure_retry_and_authenticated_responses() {
        let fixture = pairing_fixture(RELAY_KEY_ID);
        let lease = lease_context(&fixture.reservation);
        let (a_keys, b_keys) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let capabilities = vec!["encrypted_frames_v1".to_owned(), "task_v1".to_owned()];
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation.clone(),
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7; 32],
                    transport_key_fingerprint: [9; 32],
                    negotiated_capabilities: capabilities.clone(),
                },
            )
            .unwrap();
        let (sender, mut receiver) = transport_channels(
            ControllerRole::ControllerA,
            lease.remote_control_generation.clone(),
            1,
            1,
            8192,
            1024,
        );
        sender
            .install_authenticated_lease(AuthenticatedLease {
                context: lease.clone(),
                negotiated_capabilities: capabilities.clone(),
            })
            .unwrap();
        manager.replace_transport(sender);
        let business = ControllingLaneState::default();
        business.install(HandshakeRole::A, &lease, a_keys).unwrap();
        let mut responder = EncryptedLane::new(HandshakeRole::B, &lease, b_keys).unwrap();
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .unwrap()
            .0;
        let expected = manager.authenticated_remote_proxy_identity().unwrap();
        let transport_errors = RefCell::new(Vec::new());
        let submitted_ids = RefCell::new(Vec::new());
        let send = |payload: &Value| {
            submitted_ids
                .borrow_mut()
                .push(payload["request_id"].as_str().unwrap().to_owned());
            manager.send_capability_for_owner(&operation, &expected, &business, payload, |error| {
                transport_errors.borrow_mut().push(error);
                "controller_lane_unavailable".to_owned()
            })
        };
        let old = sessions
            .capability_request_with_send(&session.session_ref, &lease, send)
            .unwrap();
        for _ in 0..6 {
            let failure = sessions
                .capability_request_with_send(&session.session_ref, &lease, send)
                .unwrap_err();
            assert_eq!(failure.into_code(), "controller_lane_unavailable");
            assert_eq!(
                sessions
                    .pending_capability_ids_for_test(&session.session_ref)
                    .unwrap(),
                vec![old.request_id.clone()],
                "a failed enqueue must neither retain its ID nor remove the accepted request"
            );
        }
        assert_eq!(
            transport_errors.borrow().as_slice(),
            vec![
                super::super::super::relay_transport::ControllerSendError::QueueFull(
                    super::super::super::relay_transport::FramePriority::Business,
                );
                6
            ]
            .as_slice(),
        );
        assert_eq!(submitted_ids.borrow().len(), 7);
        let old_frame: Value =
            serde_json::from_str(&receiver.take_business_for_test().unwrap()).unwrap();
        let (kind, old_payload) = responder.open(&old_frame, &capabilities).unwrap();
        assert_eq!(kind, "capability_snapshot_request");
        assert_eq!(old_payload["request_id"], old.request_id);
        let new = sessions
            .capability_request_with_send(&session.session_ref, &lease, send)
            .unwrap();
        assert_eq!(
            sessions
                .pending_capability_ids_for_test(&session.session_ref)
                .unwrap(),
            vec![old.request_id.clone(), new.request_id.clone()],
        );
        assert!(
            submitted_ids.borrow()[1..7]
                .iter()
                .all(|id| id != &old.request_id && id != &new.request_id)
        );
        let new_frame: Value =
            serde_json::from_str(&receiver.take_business_for_test().unwrap()).unwrap();
        let (kind, new_payload) = responder.open(&new_frame, &capabilities).unwrap();
        assert_eq!(kind, "capability_snapshot_request");
        assert_eq!(new_payload["request_id"], new.request_id);
        assert_ne!(old.request_id, new.request_id);

        let snapshot = |request: &Value| {
            json!({
                "v": 1, "type": "capability_snapshot", "connection_id": lease.connection_id,
                "controller_lease_id": lease.controller_lease_id, "lease_epoch": lease.lease_epoch,
                "relay_epoch": lease.relay_epoch, "remote_control_generation": lease.remote_control_generation,
                "request_id": request["request_id"], "session_id": request["session_id"],
                "controlled_session_id": "b-session-1", "capability_revision": "4", "expires_at_ms": "30000",
                "modes": ["chat"], "tools": [], "skills": [], "workspaces": [],
            })
        };
        let old_reply = responder
            .seal(
                "capability_snapshot",
                &snapshot(&old_payload),
                &capabilities,
            )
            .unwrap();
        let new_reply = responder
            .seal(
                "capability_snapshot",
                &snapshot(&new_payload),
                &capabilities,
            )
            .unwrap();
        let projections = std::sync::Mutex::new(Vec::new());
        let mappings = std::sync::Mutex::new(Vec::new());
        let mut sinks = ControllingLaneSinks {
            on_status: &mut |_| {},
            on_session: &mut |projection| projections.lock().unwrap().push(projection.clone()),
            on_mapping: &mut |_, remote_ref, controlled_id| {
                mappings
                    .lock()
                    .unwrap()
                    .push((remote_ref.to_owned(), controlled_id.to_owned()));
                Ok(())
            },
            on_task: &mut |_, _| {},
            on_journal: &mut |_| Ok(()),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut |_| false,
        };
        let mut journal = JournalExpectations::default();
        let mut liveness = LivenessExpectations::default();
        dispatch_inbound_frame(
            &manager,
            &operation,
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &old_reply.to_string(),
            &mut sinks,
        );
        assert!(projections.lock().unwrap().is_empty());
        assert!(mappings.lock().unwrap().is_empty());
        assert_eq!(
            sessions
                .pending_capability_ids_for_test(&session.session_ref)
                .unwrap(),
            vec![old.request_id.clone(), new.request_id.clone()],
        );
        dispatch_inbound_frame(
            &manager,
            &operation,
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &new_reply.to_string(),
            &mut sinks,
        );
        assert_eq!(
            mappings.lock().unwrap().as_slice(),
            &[(session.session_ref.clone(), "b-session-1".to_owned())]
        );
        assert_eq!(projections.lock().unwrap().len(), 1);
        assert_eq!(
            projections.lock().unwrap()[0]
                .capability_request_id
                .as_deref(),
            Some(new.request_id.as_str())
        );
        assert_eq!(projections.lock().unwrap()[0].capability_revision, Some(4));
        assert!(
            sessions
                .pending_capability_ids_for_test(&session.session_ref)
                .unwrap()
                .is_empty()
        );
        dispatch_inbound_frame(
            &manager,
            &operation,
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &new_reply.to_string(),
            &mut sinks,
        );
        assert_eq!(projections.lock().unwrap().len(), 1);
    }

    #[test]
    fn opened_capability_error_cannot_mutate_a_replacement_owner_with_identical_lease() {
        let fixture = pairing_fixture(RELAY_KEY_ID);
        let lease = lease_context(&fixture.reservation);
        let (a_keys, b_keys) =
            paired_session_keys(fixture.a_keys, fixture.b_keys, &fixture.reservation);
        let capabilities = vec!["encrypted_frames_v1".into(), "task_v1".into()];
        let manager = ControllingDeviceManager::default();
        let connect = || {
            let operation = manager.connect("234-4567").unwrap();
            manager.complete_reserved(operation.clone()).unwrap();
            manager
                .complete_connected(
                    operation.clone(),
                    ControllerLease {
                        context: lease.clone(),
                        peer_endpoint_id: "endpoint-0001".into(),
                        reservation_id: "reservation-0001".into(),
                        device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                        liveness_epoch_a: "live-a-1".into(),
                        liveness_epoch_b: "live-b-1".into(),
                        resume_deadline_b: 30_000,
                        resume_token: [7; 32],
                        transport_key_fingerprint: [9; 32],
                        negotiated_capabilities: capabilities.clone(),
                    },
                )
                .unwrap();
            operation
        };
        let old = connect();
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .unwrap()
            .0;
        let request = sessions
            .capability_request(&session.session_ref, &lease)
            .unwrap();
        let payload = json!({ "v":1,"type":"capability_snapshot_error", "connection_id":lease.connection_id,
            "controller_lease_id":lease.controller_lease_id,"lease_epoch":lease.lease_epoch,"relay_epoch":lease.relay_epoch,
            "remote_control_generation":lease.remote_control_generation,"request_id":request.request_id,"session_id":session.session_id,
            "code":"capability_snapshot_unavailable" });
        let business = ControllingLaneState::default();
        business.install(HandshakeRole::A, &lease, a_keys).unwrap();
        let mut b = EncryptedLane::new(HandshakeRole::B, &lease, b_keys).unwrap();
        let encrypted = b
            .seal("capability_snapshot_error", &payload, &capabilities)
            .unwrap();
        let (kind, opened) = manager
            .with_lane_owner(&old, || business.open(&encrypted, &capabilities))
            .unwrap()
            .unwrap();
        // The old frame passed AEAD before the singleton was replaced. The
        // intent/request deliberately remain pending with identical lease bytes.
        manager.disconnect();
        let _new = connect();
        let mut sinks = ControllingLaneSinks {
            on_status: &mut |_| {},
            on_session: &mut |_| panic!("stale capability error projected"),
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut |_, _| {},
            on_journal: &mut |_| Ok(()),
            on_snapshot: &mut |_| Ok(()),
            on_liveness: &mut |_| false,
        };
        dispatch_authenticated_payload(
            &manager,
            &old,
            &sessions,
            &business,
            &mut JournalExpectations::default(),
            &lease,
            &capabilities,
            &kind,
            opened,
            &mut sinks,
        );
        let pending = sessions
            .fail_pending_capability_for_lease(&lease, "capability_snapshot_unavailable")
            .unwrap();
        assert_eq!(
            pending.capability_request_id.as_deref(),
            Some(request.request_id.as_str())
        );
    }

    /// Uncorrelated relay errors must not consume a capability request.
    #[test]
    fn uncorrelated_relay_error_does_not_consume_current_or_reopened_capability_request() {
        let fixture = pairing_fixture(RELAY_KEY_ID);
        let lease = lease_context(&fixture.reservation);
        let capabilities = vec!["encrypted_frames_v1".to_owned(), "task_v1".to_owned()];
        let manager = ControllingDeviceManager::default();
        let operation = manager.connect("234-4567").unwrap();
        manager.complete_reserved(operation.clone()).unwrap();
        manager
            .complete_connected(
                operation.clone(),
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-1".into(),
                    liveness_epoch_b: "live-b-1".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7_u8; 32],
                    transport_key_fingerprint: [9_u8; 32],
                    negotiated_capabilities: capabilities.clone(),
                },
            )
            .unwrap();
        let sessions = ControllingSessionManager::default();
        let session = sessions
            .open_with_outcome(None, "Session", "chat", &lease, &capabilities)
            .map(|(projection, _)| projection)
            .unwrap();
        let request = sessions
            .capability_request(&session.session_ref, &lease)
            .unwrap();
        let business = ControllingLaneState::default();
        let mut projections = Vec::new();
        let mut on_status = |_: &ControllingStatus| {};
        let mut on_session =
            |projection: &ControllingSessionProjection| projections.push(projection.clone());
        let mut on_task = |_: &LeaseContext, _: Value| {};
        let mut on_journal = |_: &AuthenticatedJournalFrame<'_>| Ok(());
        let mut on_snapshot = |_: &AuthenticatedSnapshotFrame<'_>| Ok(());
        let mut on_liveness = |_: Value| false;
        let mut sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut on_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        let mut journal = JournalExpectations::default();
        let mut liveness = LivenessExpectations::default();
        dispatch_inbound_frame(
            &manager,
            &manager.current_operation().unwrap(),
            &sessions,
            &business,
            &mut journal,
            &mut liveness,
            &json!({
                "v": 1,
                "type": "error",
                "request_id": "request-invalid",
                "code": "invalid_message",
                "message": "invalid message",
            })
            .to_string(),
            &mut sinks,
        );
        drop(sinks);

        assert!(projections.is_empty());
        let still_pending = sessions
            .fail_pending_capability_for_lease(&lease, "capability_snapshot_unavailable")
            .unwrap();
        assert_eq!(
            still_pending.capability_request_id.as_deref(),
            Some(request.request_id.as_str())
        );
        // Reopen under the same operation and lease. A late error for the old
        // encrypted frame has no request ID and cannot retire the new request.
        sessions.close(&session.session_ref).unwrap();
        sessions
            .open_with_outcome(
                Some(&session.session_ref),
                "Session",
                "chat",
                &lease,
                &capabilities,
            )
            .unwrap();
        let reopened = sessions
            .capability_request(&session.session_ref, &lease)
            .unwrap();
        let mut same_owner_session = |_: &ControllingSessionProjection| {
            panic!("uncorrelated relay error reached new session")
        };
        let mut same_sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut same_owner_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        dispatch_inbound_frame(&manager, &operation, &sessions, &business, &mut journal, &mut liveness,
            &json!({"v":1,"type":"error","request_id":"request-invalid","code":"invalid_message","message":"invalid message"}).to_string(), &mut same_sinks);
        drop(same_sinks);
        let still_pending = sessions
            .fail_pending_capability_for_lease(&lease, "capability_snapshot_unavailable")
            .unwrap();
        assert_eq!(
            still_pending.capability_request_id.as_deref(),
            Some(reopened.request_id.as_str())
        );
        // Replacement can happen after the keeper's cheap event fence. Reuse
        // the same lease bytes deliberately: only the original operation owns
        // this plaintext error, and must not consume the new pending request.
        manager.with_lane_owner(&operation, || ()).unwrap();
        manager.disconnect();
        let replacement = manager.connect("234-4567").unwrap();
        manager.complete_reserved(replacement.clone()).unwrap();
        manager
            .complete_connected(
                replacement,
                ControllerLease {
                    context: lease.clone(),
                    peer_endpoint_id: "endpoint-0001".into(),
                    reservation_id: "reservation-0001".into(),
                    device_display_name: DEVICE_DISPLAY_NAME_PLACEHOLDER.to_owned(),
                    liveness_epoch_a: "live-a-2".into(),
                    liveness_epoch_b: "live-b-2".into(),
                    resume_deadline_b: 30_000,
                    resume_token: [7; 32],
                    transport_key_fingerprint: [9; 32],
                    negotiated_capabilities: capabilities.clone(),
                },
            )
            .unwrap();
        let next_request = sessions
            .capability_request(&session.session_ref, &lease)
            .unwrap();
        let mut ignored_session =
            |_: &ControllingSessionProjection| panic!("old error reached new owner");
        let mut stale_sinks = ControllingLaneSinks {
            on_status: &mut on_status,
            on_session: &mut ignored_session,
            on_mapping: &mut |_, _, _| Ok(()),
            on_task: &mut on_task,
            on_journal: &mut on_journal,
            on_snapshot: &mut on_snapshot,
            on_liveness: &mut on_liveness,
        };
        dispatch_inbound_frame(&manager, &operation, &sessions, &business, &mut journal, &mut liveness,
            &json!({"v":1,"type":"error","request_id":"old-error","code":"invalid_message","message":"invalid message"}).to_string(), &mut stale_sinks);
        let still_pending = sessions
            .fail_pending_capability_for_lease(&lease, "capability_snapshot_unavailable")
            .unwrap();
        assert_eq!(
            still_pending.capability_request_id.as_deref(),
            Some(next_request.request_id.as_str())
        );
        assert!(!request_journal_replay(
            &manager,
            &operation,
            &business,
            &lease,
            &capabilities,
            &session.session_ref,
            "journal-epoch",
            0,
            "old-replay"
        ));
    }
}
