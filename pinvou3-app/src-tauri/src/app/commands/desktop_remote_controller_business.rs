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
            crate::features::remote_control::controller::relay_runtime::controlled_monotonic_millis(),
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
}

/// B 端是否仍持有活租约（供 worker 在派发前自检；当前由设备所有者状态判断）。
#[allow(dead_code)]
pub(crate) fn controlled_is_live(app: &AppHandle) -> bool {
    app.state::<ControlledDeviceManager>()
        .live_runtime_context()
        .is_ok()
}
