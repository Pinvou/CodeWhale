//! B 端业务端口实现（E2E-04）。
//!
//! 握手完成后的 worker 只负责解密/映射/回传，真正的业务动作（能力快照、
//! 任务接纳、任务取消）在这里落到既有的命令层实现上：三条路径都复用
//! `desktop_remote_task` / `desktop_remote_capabilities` 的同一套逻辑，避免
//! 「命令入口」与「wire 入口」两份实现漂移。

use serde_json::Value;

use crate::features::remote_control::controller::capabilities::CapabilitySnapshotManager;
use crate::features::remote_control::controller::controlled::{
    ControlledCleanupScope, ControlledCleanupSink, ControlledDeviceManager,
};
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::relay_runtime::{
    ControlledBusinessPort, controlled_monotonic_millis,
};
use base64::Engine as _;
use std::sync::Arc;
use tauri::{AppHandle, Manager as _};

/// E2E-06：B 端清理端口。`ControlledDeviceManager` 在 stop / refresh / 远端断开 /
/// 恢复失败等任何终态都会触发它；这里用同一代的任务登记表中断本代远端任务，
/// 任务生命周期钩子会顺带回收临时模型凭证、待决权限与传输暂存
/// （`controller/task.rs` 的 `RemoteTaskService::cleanup`）。
pub(crate) struct DesktopControlledCleanup {
    app: AppHandle,
}

impl DesktopControlledCleanup {
    pub(crate) fn new(app: AppHandle) -> Self {
        Self { app }
    }
}

impl ControlledCleanupSink for DesktopControlledCleanup {
    fn cleanup(&self, generation: &str, scope: ControlledCleanupScope) {
        if scope.session_keys {
            if let Some(relay) = self
                .app
                .try_state::<crate::features::remote_control::RemoteControlManager>()
            {
                relay.stop_controller_relay(generation);
            }
        }
        // 审核修复：只有涉及远端任务/待决权限/临时模型的清理才需要中断任务；
        // 纯凭证（session_keys/workspace_grants）清理不得顺带打断任务。
        if !(scope.remote_tasks || scope.pending_permissions || scope.temporary_models) {
            return;
        }
        let Some(runtime) = self
            .app
            .try_state::<Arc<super::desktop_remote_task::DesktopRemoteTaskRuntime>>()
        else {
            // 任务运行时未装配（例如单元测试环境）：本代没有任何远端任务需要清理。
            return;
        };
        let now_b = controlled_monotonic_millis();
        let _ = runtime.service().interrupt_generation_and_clear_sessions(
            generation,
            "remote_control_stopped",
            now_b,
        );
    }
}

/// 绑定 app handle 的 B 端业务端口（每个连接一个实例）。
pub(crate) struct DesktopControlledBusiness {
    pub(crate) app: AppHandle,
}

#[async_trait::async_trait]
impl ControlledBusinessPort for DesktopControlledBusiness {
    async fn capability_snapshot(
        &self,
        lease: &LeaseContext,
        request_id: &str,
        session_id: Option<&str>,
        known_revision: u64,
    ) -> Result<Value, String> {
        let controlled_session =
            if let Some(session_ref) = session_id.filter(|value| !value.is_empty()) {
                let authoritative_id = super::desktop_remote_task::ensure_controlled_session(
                    &self.app,
                    session_ref,
                    "",
                    false,
                    &lease.remote_control_generation,
                )?;
                Some((session_ref.to_owned(), authoritative_id))
            } else {
                None
            };
        let capabilities = self.app.state::<CapabilitySnapshotManager>();
        let now_b =
            crate::features::remote_control::controller::relay_runtime::controlled_monotonic_millis(
            );
        // 只复用刚生成、且比 A 已知 revision 更新的快照：这能吸收紧邻的重复
        // 请求，又不会把临近过期的快照当作完整 30 秒租期返回给兼容旧报文的 A。
        if let Ok(current) = capabilities.current_for_context(lease, now_b) {
            let payload = capabilities.wire_capability_snapshot(&current, request_id);
            let revision = payload["capability_revision"]
                .as_str()
                .and_then(|value| value.parse::<u64>().ok())
                .unwrap_or(0);
            let remaining_ms = payload["expires_at_ms"]
                .as_str()
                .and_then(|value| value.parse::<u64>().ok())
                .unwrap_or(0)
                .saturating_sub(now_b);
            if revision > known_revision && remaining_ms >= 29_000 {
                if let Some((session_ref, authoritative_id)) = controlled_session.as_ref() {
                    super::desktop_remote_task::project_controlled_session(
                        &self.app,
                        session_ref,
                        authoritative_id,
                        lease,
                    )?;
                }
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "capability_snapshot_ready",
                    "cached",
                );
                return Ok(payload);
            }
        }
        let snapshot = match super::desktop_remote_capabilities::build_capability_snapshot(
            &self.app,
        )
        .await
        {
            Ok(snapshot) => snapshot,
            Err(error) => {
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "capability_snapshot_failed",
                    &error,
                );
                return Err(error);
            }
        };
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "capability_snapshot_ready",
            "fresh",
        );
        if let Some((session_ref, authoritative_id)) = controlled_session.as_ref() {
            super::desktop_remote_task::project_controlled_session(
                &self.app,
                session_ref,
                authoritative_id,
                lease,
            )?;
        }
        Ok(capabilities.wire_capability_snapshot(&snapshot, request_id))
    }

    async fn submit_task(
        &self,
        _lease: &LeaseContext,
        session_ref: &str,
        payload: &Value,
    ) -> Result<Value, String> {
        crate::features::remote_control::controller::relay_runtime::log_controlled_event(
            "task_submit_business_entered",
            "validating",
        );
        let result =
            super::desktop_remote_task::submit_decrypted_task(&self.app, session_ref, payload);
        match &result {
            Ok(_) => {
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "task_submit_business_accepted",
                    "admitted",
                )
            }
            Err(error) => {
                crate::features::remote_control::controller::relay_runtime::log_controlled_event(
                    "task_submit_business_rejected",
                    error,
                )
            }
        }
        result
    }

    async fn cancel_task(
        &self,
        _lease: &LeaseContext,
        _session_ref: &str,
        task_id: &str,
    ) -> Result<Option<Value>, String> {
        super::desktop_remote_task::cancel_decrypted_task(&self.app, task_id).map(Some)
    }

    async fn replay_journal(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        journal_epoch: &str,
        after_sequence: u64,
    ) -> Result<Vec<(String, Value)>, String> {
        let history = self
            .app
            .state::<std::sync::Arc<super::desktop_remote_history::DesktopRemoteHistoryRuntime>>();
        Ok(
            match history.journal().replay_after(journal_epoch, after_sequence) {
                crate::features::remote_control::controller::journal::ReplayOutcome::Events(events) => {
                    events
                        .into_iter()
                        .filter_map(|event| {
                            crate::features::remote_control::controller::relay_runtime::build_journal_event_payload(
                                lease,
                                session_ref,
                                &event,
                            )
                            .map(|payload| ("journal_event".to_owned(), payload))
                        })
                        .take(MAX_REPLAY_FRAMES)
                        .collect()
                }
                crate::features::remote_control::controller::journal::ReplayOutcome::Gap(snapshot) => {
                    vec![(
                        "session_snapshot".to_owned(),
                        build_session_snapshot_payload(lease, session_ref, "request-replay-gap", &snapshot)?,
                    )]
                }
            },
        )
    }

    async fn session_snapshot(
        &self,
        lease: &LeaseContext,
        session_ref: &str,
        request_id: &str,
    ) -> Result<Value, String> {
        let history = self
            .app
            .state::<std::sync::Arc<super::desktop_remote_history::DesktopRemoteHistoryRuntime>>();
        let snapshot = history.journal().snapshot();
        build_session_snapshot_payload(lease, session_ref, request_id, &snapshot)
    }
}

/// 一次回放最多回传的帧数：journal 保留窗口有界，但仍给回放本身一个硬上限。
const MAX_REPLAY_FRAMES: usize = 256;

/// 构造 wire `session_snapshot`（E2E-05 缺口恢复）：快照本体 base64 + 摘要，
/// 会话身份使用 A 的引用。
fn build_session_snapshot_payload(
    lease: &LeaseContext,
    session_ref: &str,
    request_id: &str,
    snapshot: &crate::features::remote_control::controller::journal::JournalSnapshot,
) -> Result<Value, String> {
    // Keep the authenticated snapshot body on the same A-generated session
    // identity as incremental journal events. The snapshot digest does not
    // include session ids, so the authoritative continuity proof is unchanged.
    let mut wire_snapshot = snapshot.clone();
    for event in &mut wire_snapshot.events {
        event.session_id = session_ref.to_owned();
    }
    let raw =
        serde_json::to_vec(&wire_snapshot).map_err(|_| "journal_snapshot_invalid".to_owned())?;
    let digest = wire_snapshot.digest.clone();
    if digest.len() != 64 {
        return Err("journal_snapshot_invalid".to_owned());
    }
    Ok(serde_json::json!({
        "v": 1,
        "type": "session_snapshot",
        "connection_id": lease.connection_id,
        "controller_lease_id": lease.controller_lease_id,
        "lease_epoch": lease.lease_epoch,
        "relay_epoch": lease.relay_epoch,
        "remote_control_generation": lease.remote_control_generation,
        "request_id": request_id,
        "session_id": session_ref,
        "journal_epoch": wire_snapshot.epoch,
        "base_sequence": wire_snapshot.sequence.to_string(),
        "snapshot_b64": base64::engine::general_purpose::STANDARD.encode(&raw),
        "snapshot_bytes": raw.len().to_string(),
        "sha256": digest,
    }))
}

/// B 端是否仍持有活租约（供 worker 在派发前自检；当前由设备所有者状态判断）。
#[allow(dead_code)]
pub(crate) fn controlled_is_live(app: &AppHandle) -> bool {
    app.state::<ControlledDeviceManager>()
        .live_runtime_context()
        .is_ok()
}
