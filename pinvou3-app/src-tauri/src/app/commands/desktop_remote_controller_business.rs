//! B 端业务端口实现（E2E-04）。
//!
//! 握手完成后的 worker 只负责解密/映射/回传，真正的业务动作（能力快照、
//! 任务接纳、任务取消）在这里落到既有的命令层实现上：三条路径都复用
//! `desktop_remote_task` / `desktop_remote_capabilities` 的同一套逻辑，避免
//! 「命令入口」与「wire 入口」两份实现漂移。

use serde_json::Value;

use crate::features::remote_control::controller::capabilities::CapabilitySnapshotManager;
use crate::features::remote_control::controller::controlled::ControlledDeviceManager;
use crate::features::remote_control::controller::protocol::LeaseContext;
use crate::features::remote_control::controller::relay_runtime::ControlledBusinessPort;
use base64::Engine as _;
use tauri::{AppHandle, Manager as _};

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
        known_revision: u64,
    ) -> Result<Value, String> {
        let capabilities = self.app.state::<CapabilitySnapshotManager>();
        // 本地已持有同一 revision 且未过期时直接复用，避免每次请求都抬 revision。
        if let Ok(current) = capabilities.validate(
            &known_revision.to_string(),
            lease,
            crate::features::remote_control::controller::relay_runtime::controlled_monotonic_millis(
            ),
        ) {
            if known_revision > 0 {
                return Ok(capabilities.wire_capability_snapshot(&current, request_id));
            }
        }
        let snapshot =
            super::desktop_remote_capabilities::build_capability_snapshot(&self.app).await?;
        Ok(capabilities.wire_capability_snapshot(&snapshot, request_id))
    }

    async fn submit_task(
        &self,
        _lease: &LeaseContext,
        session_ref: &str,
        payload: &Value,
    ) -> Result<Value, String> {
        super::desktop_remote_task::submit_decrypted_task(&self.app, session_ref, payload)
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
        let history =
            self.app.state::<std::sync::Arc<super::desktop_remote_history::DesktopRemoteHistoryRuntime>>();
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
        let history =
            self.app.state::<std::sync::Arc<super::desktop_remote_history::DesktopRemoteHistoryRuntime>>();
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
    let raw = serde_json::to_vec(snapshot).map_err(|_| "journal_snapshot_invalid".to_owned())?;
    let digest = snapshot.digest.clone();
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
        "journal_epoch": snapshot.epoch,
        "base_sequence": snapshot.sequence.to_string(),
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
