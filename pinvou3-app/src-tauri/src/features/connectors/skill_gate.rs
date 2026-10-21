//! Skill-gate shared by the four CLI connectors (tmeet/dingtalk/feishu/
//! wecom): a per-connector disabled-flag file plus skill apply/discard.
//!
//! 每个连接器模块在自己的文件里声明 [`ConnectorSkillGate`] 实现(deny-first
//! 的 apply/refresh 走 trait 的 `apply_with_consent`)与 [`ConnectorGate`]
//! 表项(就绪探测指向本模块函数);`*_skills_state` 命令公共体收编为
//! [`ConnectorGate`] 的公共方法,启动刷新由 `connector_cli` 的 GateEntry
//! 表驱动各连接器的 `*_gate_step`。
//!
//! Disable semantics: the `~/.pinvou3/<id>_disabled` file existing means the
//! user manually disabled that connector's skills, orthogonal to connection
//! state (auth). The write side (`set_disabled_flag`) was removed together
//! with the retired `set_*_enabled` commands; connector switches now persist
//! through the unified scope state (`set_disabled_connectors` → marketplace
//! scope), so the gate only reads the flag.

use std::path::PathBuf;

use serde_json::{Value, json};
/// Retain error origin: callers may localize materialization, never consent.
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum SkillApplyError {
    Consent(String),
    Materialization(String),
}

impl SkillApplyError {
    pub(crate) fn map_materialization(self, map: impl FnOnce(String) -> String) -> String {
        match self {
            Self::Consent(error) => error,
            Self::Materialization(error) => map(error),
        }
    }
}

/// The visibility transition owns deny-first ordering, not individual commands.
/// Keep registration in the caller's existing blocking boundary.
fn apply_registered_skills<G: ConnectorSkillGate + ?Sized>(
    gate: &G,
    visible: bool,
    register: impl FnOnce(&str, bool) -> Result<(), String>,
) -> Result<(), SkillApplyError> {
    register(gate.id(), visible).map_err(SkillApplyError::Consent)?;
    gate.apply_skills(visible)
        .map_err(SkillApplyError::Materialization)
}

pub(crate) trait ConnectorSkillGate {
    /// 连接器 id(事件前缀 / 日志标签,如 `"tmeet"`)。
    fn id(&self) -> &'static str;

    /// 停用标志文件名(如 `"tmeet_disabled"`)。
    fn disabled_filename(&self) -> &'static str;

    /// 按 `visible` 增 / 删本连接器的技能文件 —— 调各自的
    /// `Pinvou3Bundle::apply_*_skills`。返回 `Result` 以传播写盘失败。
    fn apply_skills(&self, visible: bool) -> Result<(), String>;

    fn apply_with_consent(&self, visible: bool) -> Result<(), SkillApplyError> {
        apply_registered_skills(
            self,
            visible,
            crate::features::marketplace::deny_first_register_connector,
        )
    }

    /// 停用标志文件完整路径:`~/.pinvou3/<disabled_filename>`。
    fn disabled_path(&self) -> PathBuf {
        crate::platform::paths::pinvou3_home().join(self.disabled_filename())
    }

    /// 是否被手动停用(停用标志文件存在即停用)。
    fn is_disabled(&self) -> bool {
        self.disabled_path().exists()
    }
}

/// 单个 CLI 连接器的门控表项(skills_state 命令公共体 + 停用标志路径/读侧
/// 判定)。deny-first 的 apply/refresh 走各连接器的 [`ConnectorSkillGate`]
/// 实现,启动刷新由 `connector_cli` 的 GateEntry 表驱动。
pub(crate) struct ConnectorGate {
    /// 停用标志文件名(如 `"tmeet_disabled"`)。
    pub disabled_filename: &'static str,
    /// 实时就绪探测(spawn 对应 CLI 查 auth 状态;未装返回 false)。
    pub ready_probe: fn() -> bool,
}

impl ConnectorGate {
    /// 停用标志文件完整路径:`~/.pinvou3/<disabled_filename>`。
    pub fn disabled_path(&self) -> PathBuf {
        crate::platform::paths::pinvou3_home().join(self.disabled_filename)
    }

    /// 是否被手动停用(停用标志文件存在即停用)。与连接状态正交。
    pub fn is_disabled(&self) -> bool {
        self.disabled_path().exists()
    }

    /// `*_skills_state` 命令公共体:给前端渲染开关态
    /// `{connected, enabled(=未停用), visible(=connected&&enabled)}`。
    pub async fn skills_state_command(&'static self) -> Result<Value, String> {
        tokio::task::spawn_blocking(|| {
            let disabled = self.is_disabled();
            let connected = (self.ready_probe)();
            Ok::<Value, String>(json!({
                "connected": connected,
                "enabled": !disabled,
                "visible": connected && !disabled,
            }))
        })
        .await
        .map_err(|e| format!("spawn_blocking: {e}"))?
    }
}

/// 连接器「转为可用」后的默认关同意同步 —— review #455 R13-B3 的 fail-visible
/// 契约的落点（上游集中在 `ConnectorGate::apply_skills_command`，本树的
/// Official trait 设计下由四个连接器的 `*_apply_skills` / `*_gate_step`
/// 共用）：吞掉错误会让连接器在零同意状态下对模型可见。
/// 错误文案由 `scope::consent_sync_failure_message` 单源生成（携带前端共享
/// 标记 `scope::CONSENT_SYNC_FAILURE_MARKER`，两侧由
/// consent_marker_frontend.test.mjs 与 Rust 发射端 pin 住），并把原始原因记入日志。
pub(crate) fn apply_skills_consent_sync(id: &str) -> Result<(), String> {
    register_installed_skill_consent(id).map_err(|e| {
        log::warn!("[{id}] persisting the default-off consent state failed: {e}");
        crate::features::marketplace::scope::consent_sync_failure_message(
            &format!("{id} connected"),
            &e,
        )
    })
}

/// Raw install registration, also used before IMA touches any credential.
/// It is NOT the CLI visibility registration: installed-skill reconnect skips
/// known bundles and preserves its original persistence error without wrapping.
pub(crate) fn register_installed_skill_consent(id: &str) -> Result<(), String> {
    crate::features::marketplace::sync_deny_all_scopes_after_install(id)
}

/// 启动期门控刷新的 LEDGER-GATED 同步（review #455 round-31 BLOCKER）：
/// 只对从未同步过的 `(scope, pack)` 对补写默认关行——已连接连接器的刷新
/// `show` 恒为 true（旧停用标志只读），无条件推送会在每次开机把用户显式
/// 开启的行加回去。失败标记到启动时间线（可观测）并上抛给调用方。
pub(crate) fn refresh_gate_consent_sync(id: &str, display_name: &str) -> Result<(), String> {
    crate::features::marketplace::sync_deny_all_scopes_refresh(id).map_err(|e| {
        crate::platform::startup::mark_with_detail(
            "rust",
            "connector_consent_sync:failed",
            &format!("{id}: {e}"),
        );
        format!("{}技能门控刷新后的默认关同意同步失败: {e}", display_name)
    })
}
#[cfg(test)]
mod tests {
    use super::*;

    /// Minimal hand-built gate driving the default implementations: exercises
    /// the disabled-path derivation under a temporary `PINVOU3_HOME`.
    fn fake_gate() -> ConnectorGate {
        ConnectorGate {
            disabled_filename: "fake_disabled",
            ready_probe: || false,
        }
    }

    /// Pin both real entry points to the shared deny-first operation.
    /// Behavioral tests exercise refusals and order; any bypass must fail here.
    #[test]
    fn command_bodies_register_deny_first_before_materialization() {
        for (module, source) in [
            ("feishu", include_str!("feishu.rs")),
            ("wecom", include_str!("wecom.rs")),
            ("dingtalk", include_str!("dingtalk.rs")),
            ("tmeet", include_str!("tmeet.rs")),
        ] {
            for entry in [
                format!("pub async fn {module}_apply_skills("),
                format!("pub(crate) fn {module}_gate_step("),
            ] {
                let body = source
                    .split(&entry)
                    .nth(1)
                    .expect("real connector entry")
                    .split("\n}")
                    .next()
                    .unwrap();
                assert_eq!(
                    body.matches("GATE.apply_with_consent(show)").count(),
                    1,
                    "{module}: {entry}"
                );
                assert!(
                    !body.contains("GATE.apply_skills("),
                    "{module}: no unregistered materialization"
                );
            }
        }
    }

    #[test]
    fn shared_apply_preserves_order_refusal_visibility_and_error_origin() {
        use std::cell::RefCell;
        struct RecordingGate<'a> {
            events: &'a RefCell<Vec<(&'static str, bool)>>,
            fail: bool,
        }
        impl ConnectorSkillGate for RecordingGate<'_> {
            fn id(&self) -> &'static str {
                "recording"
            }
            fn disabled_filename(&self) -> &'static str {
                "unused"
            }
            fn apply_skills(&self, visible: bool) -> Result<(), String> {
                self.events.borrow_mut().push(("materialize", visible));
                if self.fail {
                    Err("disk-error".into())
                } else {
                    Ok(())
                }
            }
        }
        for visible in [true, false] {
            let events = RefCell::new(Vec::new());
            let gate = RecordingGate {
                events: &events,
                fail: false,
            };
            apply_registered_skills(&gate, visible, |id, show| {
                assert_eq!(id, "recording");
                events.borrow_mut().push(("register", show));
                Ok(())
            })
            .unwrap();
            assert_eq!(
                *events.borrow(),
                [("register", visible), ("materialize", visible)]
            );
            events.borrow_mut().clear();
            let error = apply_registered_skills(&gate, visible, |_, show| {
                events.borrow_mut().push(("register", show));
                Err("consent-error".into())
            })
            .unwrap_err();
            assert_eq!(
                error.map_materialization(|_| panic!("consent must stay raw")),
                "consent-error"
            );
            assert_eq!(*events.borrow(), [("register", visible)]);
        }
        let events = RefCell::new(Vec::new());
        let gate = RecordingGate {
            events: &events,
            fail: true,
        };
        let error = apply_registered_skills(&gate, true, |_, _| Ok(())).unwrap_err();
        assert_eq!(
            error.map_materialization(|e| format!("localized: {e}")),
            "localized: disk-error"
        );
    }

    /// `disabled_path` 跟随 `PINVOU3_HOME`,且文件名由 `disabled_filename` 决定。
    #[test]
    fn disabled_path_is_derived_from_pinvou3_home() {
        let _g = crate::platform::paths::tests::ENV_LOCK
            .lock()
            .unwrap_or_else(|p| p.into_inner());
        let tmp = format!(
            "{}/pinvou3-skillgate-path-{}-{}",
            std::env::temp_dir().display(),
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_nanos())
                .unwrap_or(0)
        );
        let previous = std::env::var("PINVOU3_HOME").ok();
        // SAFETY: platform::paths::tests::ENV_LOCK is held; env writes are
        // serialized in-process.
        unsafe { std::env::set_var("PINVOU3_HOME", &tmp) };

        let gate = fake_gate();
        assert_eq!(
            gate.disabled_path(),
            crate::platform::paths::pinvou3_home().join("fake_disabled")
        );
        // 标志文件不存在 → 未停用;ready_probe 恒 false → 技能不应显示。
        assert!(!gate.is_disabled());
        assert!(!(gate.ready_probe)());

        match previous {
            // SAFETY: platform::paths::tests::ENV_LOCK is held; env writes
            // are serialized in-process.
            Some(value) => unsafe { std::env::set_var("PINVOU3_HOME", value) },
            // SAFETY: platform::paths::tests::ENV_LOCK is held; env writes
            // are serialized in-process.
            None => unsafe { std::env::remove_var("PINVOU3_HOME") },
        }
        let _ = std::fs::remove_dir_all(&tmp);
    }
}
