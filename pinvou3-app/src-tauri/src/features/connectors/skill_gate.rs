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
// The connect-channel consent surface (feat/connector-connect-grants-enable).
// Imported once and re-exported for the four connector modules' gate
// implementations (they carry the outcome out of `apply_with_consent` for the
// tail + completion).
pub(crate) use crate::features::marketplace::ConnectGrantOutcome;
use crate::features::marketplace::scope::{
    connect_consent_sync_failure_message, connect_grant_commit_failure_message,
};
use crate::features::marketplace::{
    RELIED_GRANT_LOST_CAUSE, RELIED_GRANT_REPLACEMENT_PENDING_CAUSE, ReliedGrantCommit,
    complete_connect_grant, deny_first_register_connector, deny_first_register_connector_refresh,
    grant_connect_enable, grant_connect_enable_refresh, revoke_connect_enable,
    sync_deny_all_scopes_after_install,
};
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
/// Keep registration in the caller's existing blocking boundary. The gate's
/// outcome is carried out so the caller's tail leg and completion can consume
/// it; a materialization failure after a FRESH grant compensates (the M4
/// revoke, feat/connector-connect-grants-enable) so a failed connect cannot
/// leave the connector computed-ON behind a failure card.
fn apply_registered_skills<G: ConnectorSkillGate + ?Sized>(
    gate: &G,
    visible: bool,
    register: impl FnOnce(&str, bool) -> Result<ConnectGrantOutcome, String>,
) -> Result<ConnectGrantOutcome, SkillApplyError> {
    let grant = register(gate.id(), visible).map_err(SkillApplyError::Consent)?;
    gate.apply_skills(visible)
        .inspect_err(|_| {
            // M4 compensation (review fix wave): the grant above already
            // lifted the stale rows and flipped every DenyAll expansion to ON
            // — left standing behind this failure the connector would be live
            // while the card reports failure. Revoke grant + ledger and
            // restore the lifted rows so the state returns to fail-closed
            // "never consented" and a retry reruns the full connect;
            // best-effort, the apply error stays the surfaced one.
            if grant.granted {
                if let Err(revoke_err) =
                    revoke_connect_enable(&grant)
                {
                    log::warn!(
                        "[{}] revoking the connect grant after the apply failure failed: {revoke_err}",
                        gate.id()
                    );
                }
            }
        })
        .map_err(SkillApplyError::Materialization)?;
    Ok(grant)
}

pub(crate) trait ConnectorSkillGate {
    /// 连接器 id(事件前缀 / 日志标签,如 `"tmeet"`)。
    fn id(&self) -> &'static str;

    /// 停用标志文件名(如 `"tmeet_disabled"`)。
    fn disabled_filename(&self) -> &'static str;

    /// 按 `visible` 增 / 删本连接器的技能文件 —— 调各自的
    /// `Pinvou3Bundle::apply_*_skills`。返回 `Result` 以传播写盘失败。
    fn apply_skills(&self, visible: bool) -> Result<(), String>;

    /// 连接手势门(feat/connector-connect-grants-enable):`apply_skills_command`
    /// 的 deny-first 臂。返回 `ConnectGrantOutcome` 供尾腿授权与完成校验消费。
    fn apply_with_consent(&self, visible: bool) -> Result<ConnectGrantOutcome, SkillApplyError> {
        apply_registered_skills(self, visible, deny_first_register_connector)
    }

    /// 启动刷新门(refresh 变体):LEDGER-GATED 且尊重 stored 裁决——boot 刷新
    /// 背后没有用户手势,不得抬行。
    fn apply_with_consent_refresh(
        &self,
        visible: bool,
    ) -> Result<ConnectGrantOutcome, SkillApplyError> {
        apply_registered_skills(self, visible, deny_first_register_connector_refresh)
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

/// Raw install registration, also used before IMA touches any credential.
/// It is NOT the CLI visibility registration: installed-skill reconnect skips
/// known bundles and preserves its original persistence error without wrapping.
pub(crate) fn register_installed_skill_consent(id: &str) -> Result<(), String> {
    sync_deny_all_scopes_after_install(id)
}

/// 连接通道的授权尾腿（feat/connector-connect-grants-enable；上游
/// `ConnectorGate::apply_skills_command` 的 belt leg，本树 Official trait 设计
/// 下由四个连接器的 `*_apply_skills` 共用）：物化落地后补记连接启用授权，
/// 连接动作本身就是同意。门控臂已授过新鲜授权而本腿落盘失败时，先补偿撤销
/// 门控授权（round-2 review m2：不能让授权与抬行在失败卡背后存续），再以
/// 连接通道 fail-closed 文案失败——错误文案由
/// `scope::connect_consent_sync_failure_message` 单源生成（携带前端共享标记
/// `scope::CONNECT_CONSENT_SYNC_FAILURE_MARKER`，两侧由
/// consent_marker_frontend.test.mjs 与 Rust 发射端 pin 住），原始原因记入日志。
/// 返回完整 outcome（round-12 review P2）：尾腿观察到的最终同意身份是完成
/// 校验（[`complete_grant_for_completion`]）的输入，折叠成 bool 会恰好丢掉
/// round-11 替换拒绝所依赖的捕获。
pub(crate) fn grant_connect_enable_tail(
    id: &str,
    gate: &ConnectGrantOutcome,
) -> Result<ConnectGrantOutcome, String> {
    grant_connect_enable(id)
        .inspect_err(|_| {
            // Round-2 review m2: if this leg fails after the deny-first grant
            // landed, grant + lifted rows would outlive the failure while the
            // card claims "nothing was changed" — revoke (which restores
            // exactly the rows the deny-first grant lifted) so the copy is
            // true again. When deny-first did not grant (a re-show over a
            // recorded consent) nothing of this call's is at risk and the
            // standing consent must not be stripped.
            if gate.granted {
                if let Err(revoke_err) =
                    revoke_connect_enable(gate)
                {
                    log::warn!(
                        "[{id}] revoking the connect grant after the belt persist failure failed: {revoke_err}"
                    );
                }
            }
        })
        .map_err(|e| {
            log::warn!("[{id}] persisting the connect-enabled consent state failed: {e}");
            connect_consent_sync_failure_message(
                &format!("{id} connected"),
                &e,
            )
        })
}

/// 启动刷新通道的授权尾腿（grant 语义的 boot backfill；上游
/// `ConnectorGate::refresh_step` 的 tail leg，本树由四个连接器的
/// `*_gate_step` 共用）：补偿同上，失败标记到启动时间线（可观测）并上抛。
pub(crate) fn grant_connect_enable_refresh_tail(
    id: &str,
    display_name: &str,
    gate: &ConnectGrantOutcome,
) -> Result<ConnectGrantOutcome, String> {
    grant_connect_enable_refresh(id)
        .inspect_err(|_| {
            // Same round-2 m2 compensation as the connect body: a tail-leg
            // failure must not leave the backfill grant (or — nothing of this
            // call's, when deny-first already granted) standing behind a
            // failed refresh.
            if gate.granted {
                if let Err(revoke_err) =
                    revoke_connect_enable(gate)
                {
                    log::warn!(
                        "[{id}] revoking the connect grant after the refresh tail persist failure failed: {revoke_err}"
                    );
                }
            }
        })
        .map_err(|e| {
            crate::platform::startup::mark_with_detail(
                "rust",
                "connector_consent_sync:failed",
                &format!("{id}: {e}"),
            );
            format!("{}技能门控刷新后的连接启用授权落盘失败: {e}", display_name)
        })
}

/// Round-7 review P2, REQUIRED form (round-8 review), generalized by the
/// round-12 review (P2 + P3): a completion must stand on consent it can
/// PROVE permanent at completion time. The claim set is
/// [`complete_connect_grant`]'s: this operation's OWN grants (the gate's
/// and the tail's fresh grants) plus the consent its GATE relied on — the
/// decision basis. The tail's mere OBSERVATION of a different standing
/// grant is not claimable: it belongs to a newer lifecycle whose owner
/// must complete or fail on its own, and the completion declines
/// (`ReplacementPending`) rather than reporting success over an armed
/// stranger or disarming its compensation. The claims are re-validated
/// under the write lock at completion time, so the gate-era capture commits
/// only while it still stands (rounds 6/8/11), and a fresh own grant is
/// FINALIZED (the round-12 review P3).
///
/// The commit is a completion requirement, not best-effort: a
/// [`ReliedGrantCommit::ReplacementPending`] (a newer in-flight owner's
/// armed compensation can still strip the consent) or
/// [`ReliedGrantCommit::GrantLost`] (nothing stands behind the success)
/// FAILS the connect — a `visible:true` over consent that computes (or
/// can compute) OFF would be the fail-open the review forbids. A NEWER
/// owner's standing grant WITHOUT a token
/// ([`ReliedGrantCommit::Superseded`]) is deliberately tolerated: no
/// compensating revoke can match a tokenless grant, so the newer consent
/// is permanent and covers this connector. The steady state after a
/// commit is token-less, so this is a no-op write-wise for plain
/// reconnects over long-standing committed consent.
pub(crate) fn complete_grant_for_completion(
    id: &str,
    gate: &ConnectGrantOutcome,
    tail: &ConnectGrantOutcome,
) -> Result<(), String> {
    let lost =
        |cause: String| connect_consent_sync_failure_message(&format!("{id} connected"), &cause);
    match complete_connect_grant(id, gate, Some(tail)) {
        Ok(ReliedGrantCommit::NotRelied | ReliedGrantCommit::Committed) => Ok(()),
        Ok(ReliedGrantCommit::Superseded) => {
            log::info!(
                "[{id}] the relied-upon connect grant was replaced by a newer owner's committed consent; leaving it untouched"
            );
            Ok(())
        }
        // The standing grant belongs to a still-in-flight newer operation:
        // its compensation can still strip the grant, so this completion
        // must not report success. The commit wrote nothing, the newer
        // consent keeps the connector enabled, and the frontend's
        // commit-incomplete copy says exactly that without latching the
        // disabled banner (round-11 review P2).
        Ok(ReliedGrantCommit::ReplacementPending) => Err(connect_grant_commit_failure_message(
            &format!("{id} connected"),
            RELIED_GRANT_REPLACEMENT_PENDING_CAUSE,
        )),
        Ok(ReliedGrantCommit::GrantLost) => Err(lost(RELIED_GRANT_LOST_CAUSE.to_string())),
        // A persist failure is the OPPOSITE state to GrantLost: the
        // standing grant + ledger stay effective, so the connector
        // REMAINS enabled — the disabled/nothing-exposed copy of `lost`
        // would be false (round-9 review P2). The commit-stage copy says
        // so and does not drive the frontend's consent-failure latch.
        Err(commit_err) => Err(connect_grant_commit_failure_message(
            &format!("{id} connected"),
            &commit_err,
        )),
    }
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

    /// Pin both real entry points to the shared deny-first operation (the
    /// connect gesture variant in the apply command, the verdict-respecting
    /// refresh variant in the boot gate_step — feat/connector-connect-grants-
    /// enable). Behavioral tests exercise refusals and order; any bypass must
    /// fail here.
    #[test]
    fn command_bodies_register_deny_first_before_materialization() {
        for (module, source) in [
            ("feishu", include_str!("feishu.rs")),
            ("wecom", include_str!("wecom.rs")),
            ("dingtalk", include_str!("dingtalk.rs")),
            ("tmeet", include_str!("tmeet.rs")),
        ] {
            let apply_body = source
                .split(&format!("pub async fn {module}_apply_skills("))
                .nth(1)
                .expect("real connector apply entry")
                .split("\n}")
                .next()
                .unwrap();
            assert_eq!(
                apply_body.matches("GATE.apply_with_consent(show)").count(),
                1,
                "{module}: apply_skills"
            );
            assert!(
                !apply_body.contains("GATE.apply_skills("),
                "{module}: no unregistered materialization"
            );
            let refresh_body = source
                .split(&format!("pub(crate) fn {module}_gate_step("))
                .nth(1)
                .expect("real connector gate_step entry")
                .split("\n}")
                .next()
                .unwrap();
            assert_eq!(
                refresh_body
                    .matches("GATE.apply_with_consent_refresh(show)")
                    .count(),
                1,
                "{module}: gate_step must drive the verdict-respecting refresh gate"
            );
            assert!(
                !refresh_body.contains("GATE.apply_with_consent(show)"),
                "{module}: boot refresh must not use the gesture gate"
            );
            assert!(
                !refresh_body.contains("GATE.apply_skills("),
                "{module}: no unregistered materialization"
            );
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
                Ok(ConnectGrantOutcome::default())
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
        let error = apply_registered_skills(&gate, true, |_, _| Ok(ConnectGrantOutcome::default()))
            .unwrap_err();
        assert_eq!(
            error.map_materialization(|e| format!("localized: {e}")),
            "localized: disk-error"
        );
    }

    /// M4 compensation pin (feat/connector-connect-grants-enable fix wave),
    /// shared-helper level: a gate whose deny-first write landed a FRESH
    /// grant must see the grant + ledger revoked when the materialization
    /// then fails — the connector must not stay computed-ON behind a failure
    /// card. The production register closure (`deny_first_register_connector`)
    /// and the raw store are driven, so a dropped compensation or a
    /// no-op'd revoke turns the raw-store legs red.
    #[test]
    fn shared_apply_revokes_a_fresh_grant_when_materialization_fails() {
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-shared-revoke", || {
            struct FailingGate;
            impl ConnectorSkillGate for FailingGate {
                fn id(&self) -> &'static str {
                    "feishu"
                }
                fn disabled_filename(&self) -> &'static str {
                    "feishu_disabled"
                }
                fn apply_skills(&self, _: bool) -> Result<(), String> {
                    Err("injected materialization failure".into())
                }
            }
            let error = apply_registered_skills(&FailingGate, true, deny_first_register_connector)
                .unwrap_err();
            let surfaced = error.map_materialization(|e| e);
            assert!(
                surfaced.contains("injected materialization failure"),
                "the materialization failure stays the surfaced cause: {surfaced}"
            );
            let raw: serde_json::Value = serde_json::from_str(
                &std::fs::read_to_string(
                    crate::platform::paths::pinvou3_home().join("disabled_bundles.json"),
                )
                .unwrap(),
            )
            .unwrap();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_none_or(|ids| ids.iter().all(|id| id != "feishu")),
                "the fresh grant must be revoked after the materialization failure: {raw}"
            );
            assert!(
                !raw["install_default_synced"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the revoked grant must leave neither ledger pair behind: {raw}"
            );
        });
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
