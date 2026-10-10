//! Skill-gate trait shared by the four CLI connectors (tmeet/dingtalk/feishu/
//! wecom): a per-connector disabled-flag file plus skill apply/discard.
//!
//! Each connector only supplies `id`, `disabled_filename`, and `apply_skills`
//! (pointing at its `Pinvou3Bundle::apply_*_skills`); the flag-file path
//! resolution and the read-side existence check are default implementations.
//!
//! Disable semantics: the `~/.pinvou3/<id>_disabled` file existing means the
//! user manually disabled that connector's skills, orthogonal to connection
//! state (auth). The write side (`set_disabled_flag`) was removed together
//! with the retired `set_*_enabled` commands; connector switches now persist
//! through the unified scope state (`set_disabled_connectors` → marketplace
//! scope), so this trait only reads the flag.

use std::path::PathBuf;

/// Skill-gate abstraction for a single CLI connector.
///
/// Implementors supply three connector-specific items and reuse the default
/// flag-file path resolution and read-side check.
pub(crate) trait ConnectorSkillGate {
    /// 连接器 id(事件前缀 / 日志标签,如 `"tmeet"`)。
    fn id(&self) -> &'static str;

    /// 停用标志文件名(如 `"tmeet_disabled"`)。
    fn disabled_filename(&self) -> &'static str;

    /// 按 `visible` 增 / 删本连接器的技能文件 —— 调各自的
    /// `Pinvou3Bundle::apply_*_skills`。返回 `Result` 以传播写盘失败。
    fn apply_skills(&self, visible: bool) -> Result<(), String>;

    /// 停用标志文件完整路径:`~/.pinvou3/<disabled_filename>`。
    fn disabled_path(&self) -> PathBuf {
        crate::platform::paths::pinvou3_home().join(self.disabled_filename())
    }

    /// 是否被手动停用(停用标志文件存在即停用)。
    fn is_disabled(&self) -> bool {
        self.disabled_path().exists()
    }
}

/// 连接器「转为可用」后的默认关同意同步 —— review #455 R13-B3 的 fail-visible
/// 契约在 Official 四连接器 trait 设计下的落点（上游集中在
/// `ConnectorGate::apply_skills_command`，此处为等价共享实现，四个连接器的
/// `*_apply_skills` 共用）：吞掉错误会让连接器在零同意状态下对模型可见。
/// 错误文案携带前端共享标记 `scope::CONSENT_SYNC_FAILURE_MARKER`（两侧由
/// consent_marker_frontend.test.mjs 与 Rust 发射端 pin 住），并把原始原因记入日志。
pub(crate) fn apply_skills_consent_sync(id: &str) -> Result<(), String> {
    crate::features::marketplace::sync_deny_all_scopes_after_install(id).map_err(|e| {
        log::warn!("[{id}] persisting the default-off consent state failed: {e}");
        format!(
            "{id} connected, but {}: new sessions will enable it by default — turn it off in the tools list: {e}",
            crate::features::marketplace::scope::CONSENT_SYNC_FAILURE_MARKER
        )
    })
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
mod consent_sync_tests {
    use crate::features::marketplace::{
        ConnectorScope, load_disabled_bundles_for, save_disabled_bundles_for,
        sync_deny_all_scopes_after_install,
    };
    use crate::platform::test_support::with_temp_home;

    /// Round-31 BLOCKER negative control (review #455, adapted to the shared
    /// `refresh_gate_consent_sync` helper): an initialized scope, a connected
    /// connector EXPLICITLY ENABLED by the user, then the startup refresh —
    /// the stored list must still lack the id. This is the test that fails on
    /// the round-30 form (the plain membership push re-added the row at every
    /// boot, silently reverting explicit enables): the ledger-gated sync is
    /// what makes the enable sticky. Also pins the reconnect direction —
    /// teardown (exact removal) clears the ledger entry, so a fresh install /
    /// reconnect re-syncs default-off.
    #[test]
    fn refresh_gate_consent_sync_does_not_revert_explicit_enable_but_reconnect_resyncs() {
        use crate::features::marketplace::scope::remove_bundle_from_disabled_scopes_exact;

        with_temp_home("pinvou3-skillgate-ledger", || {
            // Initialize plain via the raw store shape (the migration's own
            // verdict write; the composer's first write seeds the same).
            let path = crate::platform::paths::pinvou3_home().join("disabled_bundles.json");
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(
                &path,
                r#"{"scopes":{},"initialized":["plain"],"plain_defaults_migrated":true}"#,
            )
            .unwrap();

            // First sync (install/connect equivalent): the row lands and the
            // pair is ledgered.
            sync_deny_all_scopes_after_install("connector-x").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the first sync must persist the install-default row"
            );

            // The user enables the pack: the composer whole-list write drops
            // the row + marker while the ledger entry survives.
            save_disabled_bundles_for(ConnectorScope::Plain, &[]).unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "fixture: the enable removed the row"
            );

            // The startup refresh runs for the connected connector (probe
            // true, no disable flag): the LEDGER must keep it from re-adding.
            super::refresh_gate_consent_sync("connector-x", "测试连接器").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the startup refresh must NOT revert an explicit user enable (the round-30 form fails here)"
            );

            // Reconnect direction: teardown (exact removal) clears the
            // ledger entry, so the next sync re-syncs default-off.
            remove_bundle_from_disabled_scopes_exact("connector-x").unwrap();
            sync_deny_all_scopes_after_install("connector-x").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "a fresh install / reconnect must re-sync default-off after teardown"
            );
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::platform::paths::tests::ENV_LOCK;

    /// Minimal fake impl driving the default implementations: exercises the
    /// disabled-path derivation under a temporary `PINVOU3_HOME`.
    struct FakeGate;
    impl ConnectorSkillGate for FakeGate {
        fn id(&self) -> &'static str {
            "fake"
        }
        fn disabled_filename(&self) -> &'static str {
            "fake_disabled"
        }
        fn apply_skills(&self, _visible: bool) -> Result<(), String> {
            Ok(())
        }
    }

    /// `disabled_path` 跟随 `PINVOU3_HOME`,且文件名由 `disabled_filename` 决定。
    #[test]
    fn disabled_path_is_derived_from_pinvou3_home() {
        let _g = ENV_LOCK.lock().unwrap_or_else(|p| p.into_inner());
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

        let gate = FakeGate;
        assert_eq!(
            gate.disabled_path(),
            crate::platform::paths::pinvou3_home().join("fake_disabled")
        );

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
