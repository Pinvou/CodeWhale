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
/// 错误文案由 `scope::consent_sync_failure_message` 单源生成（携带前端共享
/// 标记 `scope::CONSENT_SYNC_FAILURE_MARKER`，两侧由
/// consent_marker_frontend.test.mjs 与 Rust 发射端 pin 住），并把原始原因记入日志。
pub(crate) fn apply_skills_consent_sync(id: &str) -> Result<(), String> {
    crate::features::marketplace::sync_deny_all_scopes_after_install(id).map_err(|e| {
        log::warn!("[{id}] persisting the default-off consent state failed: {e}");
        crate::features::marketplace::scope::consent_sync_failure_message(
            &format!("{id} connected"),
            &e,
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

    /// Seed the post-migration plain-scope shape ("plain" initialized, nothing
    /// stored) — the load-bearing precondition the ledger tests share verbatim.
    fn seed_initialized_plain_scope() {
        let path = crate::platform::paths::pinvou3_home().join("disabled_bundles.json");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(
            &path,
            r#"{"scopes":{},"initialized":["plain"],"plain_defaults_migrated":true}"#,
        )
        .unwrap();
    }

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
            seed_initialized_plain_scope();

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

    /// Round-33 MAJOR 2 (review #455): the connect-path consent-persist
    /// failure copy emitted by `apply_skills_consent_sync` carries the ONE
    /// shared frontend marker (`scope::CONSENT_SYNC_FAILURE_MARKER`) — the
    /// localized template on the store cards keys on exactly this string, so
    /// a rewording here must move the frontend matcher in the same commit.
    /// Unlike ima.rs's inline-format pin, the shared emitter is drivable
    /// directly: inject the persist failure and pin the REAL error text.
    #[test]
    fn skill_gate_consent_failure_message_keeps_the_frontend_marker() {
        use crate::features::marketplace::scope::CONSENT_SYNC_FAILURE_MARKER;

        with_temp_home("pinvou3-skillgate-consent-marker", || {
            // Pre-warm: the first read persists the freeze verdict, settling
            // that write so the failpoint below hits the consent SYNC's save.
            let _ = load_disabled_bundles_for(ConnectorScope::Plain);
            // The connect-path sync's persist FAILS.
            let _failpoint =
                crate::features::marketplace::scope::fail_next_disabled_bundles_write_for_test();
            let error = super::apply_skills_consent_sync("wecom")
                .expect_err("fixture: the injected persist error must surface");
            assert!(
                error.contains(CONSENT_SYNC_FAILURE_MARKER),
                "the shipped message must carry the frontend-matched marker: {error}"
            );
            assert_eq!(
                CONSENT_SYNC_FAILURE_MARKER, "persisting their default-off consent state failed",
                "the marker value is the frontend contract (ToolStoreView consentFailure) — update both sides together"
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

    /// Seed the post-migration plain-scope shape ("plain" initialized, nothing
    /// stored) — the load-bearing precondition the ledger tests below share
    /// verbatim (517/Community suite ported onto the Official trait design).
    fn seed_initialized_plain_scope() {
        let path = crate::platform::paths::pinvou3_home().join("disabled_bundles.json");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(
            &path,
            r#"{"scopes":{},"initialized":["plain"],"plain_defaults_migrated":true}"#,
        )
        .unwrap();
    }

    /// The deny-first wiring is pinned through the four production command
    /// bodies (`*_apply_skills` / `*_gate_step`), not only through
    /// `deny_first_register_connector` driven directly — a revert of these
    /// bodies to apply-then-register would keep the behavioral suites green.
    /// Official keeps per-connector modules instead of Community's static
    /// gate table, so the order is pinned textually per module (source-side
    /// protocol, same doctrine as `protocol_tests.rs`); the behavioral
    /// "refusal lands nothing" half stays pinned by the broken-lock battery
    /// over `deny_first_register_connector` in `features::marketplace::scope`.
    #[test]
    fn command_bodies_register_deny_first_before_materialization() {
        for (module, id) in [
            ("feishu.rs", "feishu"),
            ("wecom.rs", "wecom"),
            ("dingtalk.rs", "dingtalk"),
            ("tmeet.rs", "tmeet"),
        ] {
            let source = match module {
                "feishu.rs" => include_str!("feishu.rs"),
                "wecom.rs" => include_str!("wecom.rs"),
                "dingtalk.rs" => include_str!("dingtalk.rs"),
                _ => include_str!("tmeet.rs"),
            };
            let registration = format!("deny_first_register_connector(\"{id}\", show)?;");
            let sites: Vec<usize> = source
                .match_indices(&registration)
                .map(|(offset, _)| offset)
                .collect();
            assert_eq!(
                sites.len(),
                2,
                "[{module}] the apply command and the gate step must each register deny-first: {sites:?}"
            );
            let materializations: Vec<usize> = source
                .match_indices("GATE.apply_skills(show)")
                .map(|(offset, _)| offset)
                .collect();
            assert_eq!(
                materializations.len(),
                2,
                "[{module}] expected exactly the two materialization sites: {materializations:?}"
            );
            for (registered, materialized) in sites.iter().zip(materializations.iter()) {
                assert!(
                    registered < materialized,
                    "[{module}] deny-first registration at {registered} must precede materialization at {materialized}"
                );
            }
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

    /// Round-33 MAJOR 2 (review #455): the connect-path consent-persist
    /// failure copy carries the ONE shared frontend marker
    /// (`scope::CONSENT_SYNC_FAILURE_MARKER`) — the localized template on the
    /// store cards keys on exactly this string, so a rewording here must move
    /// the frontend matcher in the same commit. The literal is asserted (not
    /// reformatted and re-matched) so a marker rename fails here instead of
    /// silently degrading the frontend guidance to generic copy.
    /// (517/Community suite port: design-agnostic, kept verbatim — the
    /// frontend pin `consent_marker_frontend.test.mjs` leans on this test.)
    #[test]
    fn skill_gate_consent_failure_message_keeps_the_frontend_marker() {
        assert_eq!(
            crate::features::marketplace::scope::CONSENT_SYNC_FAILURE_MARKER,
            "persisting their default-off consent state failed",
            "the shipped marker is the string the frontend consentFailure matcher keys on"
        );
    }

    /// Round-37 C1 (review #455): a consent persist failure between connect
    /// and enable left the pair unledgered, so the startup refresh reverted
    /// the user's enable across the restart (round-30 family residue). The
    /// explicit-enable path now RECORDS the ledger pair, closing the window:
    /// connect-sync persist fails → nothing lands → the enable succeeds →
    /// refresh must not backfill the row.
    /// (517/Community suite port: the startup-refresh consent leg drives the
    /// shared `refresh_gate_consent_sync` — the Official trait design's
    /// equivalent of upstream `ConnectorGate::refresh_step`'s ledger-gated
    /// sync leg.)
    #[test]
    fn explicit_enable_records_the_ledger_over_a_failed_connect_sync() {
        use crate::features::marketplace::scope::{
            enable_packages_in_scope, sync_deny_all_scopes_after_install,
        };
        use crate::features::marketplace::{ConnectorScope, load_disabled_bundles_for};
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-ledger-failedconnect", || {
            // Pre-warm: the first read persists the freeze verdict, settling
            // that write so the failpoint below hits the SYNC's save.
            let _ = crate::features::marketplace::scope::load_disabled_bundles_for(
                ConnectorScope::Plain,
            );
            // The connect-path sync's persist FAILS: neither the row nor the
            // ledger entry reaches disk.
            let _failpoint =
                crate::features::marketplace::scope::fail_next_disabled_bundles_write_for_test();
            assert!(
                sync_deny_all_scopes_after_install("feishu").is_err(),
                "fixture: the connect sync must fail on the injected persist error"
            );

            // The user's first enable still succeeds (nothing to remove; the
            // materialization arm seeds the scope).
            let outcome =
                enable_packages_in_scope(ConnectorScope::Plain, &["feishu".to_string()]).unwrap();
            assert!(
                outcome.state_changed,
                "fixture: the enable must materialize"
            );
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "fixture: the enable removed the id"
            );

            // The startup refresh must stay gated by the ledger the ENABLE
            // wrote — no silent default-off backfill over the enable.
            super::refresh_gate_consent_sync("feishu", "飞书").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the enable must record the ledger, or the next boot reverts it (round-30 family residue)"
            );
        });
    }

    /// Round-32 MAJOR 1 (review #455) twin negative control: the connect
    /// happens while plain is STILL UNINITIALIZED (fresh home), the user's
    /// first enable materializes the scope, and only then does the startup
    /// refresh run — the stored list must still lack the id. This fails on
    /// the round-31 form: the uninitialized connect sync recorded no ledger
    /// entry (its arm `continue`d before the write), so the refresh
    /// classified the row-absence as "never synced" and backfilled the
    /// default-off row over the enable across the restart. The fix records
    /// the pair in the uninitialized arm.
    /// (517/Community suite port: refresh consent leg via
    /// `refresh_gate_consent_sync`, see the C1 test above.)
    #[test]
    fn enable_after_uninitialized_connect_survives_startup_refresh() {
        use crate::features::marketplace::scope::{
            enable_packages_in_scope, sync_deny_all_scopes_after_install,
        };
        use crate::features::marketplace::{ConnectorScope, load_disabled_bundles_for};
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-ledger-uninit", || {
            // Fresh home: plain does not exist yet. The connector connects
            // FIRST (the connect-path sync — a user action) while plain is
            // uninitialized by design.
            sync_deny_all_scopes_after_install("feishu").unwrap();

            // The user's first composer/welcome write enables the pack: the
            // materialization arm seeds `initialized` + the expansion
            // snapshot with the id removed.
            let outcome =
                enable_packages_in_scope(ConnectorScope::Plain, &["feishu".to_string()]).unwrap();
            assert!(
                outcome.not_applied.is_empty(),
                "fixture: feishu is a builtin id and must sit in the uninitialized expansion"
            );
            assert!(
                outcome.state_changed,
                "fixture: the first enable must materialize the scope"
            );
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "fixture: the enable removed the id"
            );

            // Restart equivalent: the startup refresh runs for the connected
            // gate — the ledger entry recorded by the uninitialized connect
            // sync must keep the enable in place.
            super::refresh_gate_consent_sync("feishu", "飞书").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the startup refresh must not backfill over an enable made after an uninitialized-scope connect (round-32 MAJOR 1)"
            );
        });
    }

    /// Round-32 minor 2 (review #455): the refresh's PUSH leg itself is
    /// load-bearing and had no pin — a regression turning the refresh into a
    /// no-op kept the suite green while silently disabling the round-30 m1
    /// backfill. The refresh must backfill the default-off row for a pair no
    /// sync ever recorded, AND record the ledger entry while doing it, so a
    /// user enable right after the backfill survives the next refresh (the
    /// recording is what keeps the round-31 BLOCKER re-opened shut).
    /// (517/Community suite port: refresh consent leg via
    /// `refresh_gate_consent_sync`, see the C1 test above.)
    #[test]
    fn refresh_backfills_never_synced_row_and_records_the_ledger() {
        use crate::features::marketplace::{
            ConnectorScope, load_disabled_bundles_for, save_disabled_bundles_for,
        };
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-ledger-backfill", || {
            // Initialized plain (post-migration shape), connector-x never
            // seen by any sync: no stored row, no ledger entry.
            seed_initialized_plain_scope();

            super::refresh_gate_consent_sync("connector-x", "测试连接器").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the startup refresh must backfill the install-default row for a never-synced pair"
            );

            // The ledger entry must be recorded by that same refresh: a user
            // enable right after the backfill must survive the next refresh.
            save_disabled_bundles_for(ConnectorScope::Plain, &[]).unwrap();
            super::refresh_gate_consent_sync("connector-x", "测试连接器").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the backfill must record the ledger, or the next refresh reverts the enable (round-31 BLOCKER re-opened)"
            );
        });
    }

    /// Round-32 minor 2 (review #455): the TEARDOWN ledger-clear is what
    /// re-arms the LEDGER-GATED startup refresh. The negative control above
    /// drives its reconnect leg with the un-gated connect variant (which
    /// re-adds the row regardless), so deleting
    /// `remove_bundle_from_disabled_scopes_exact`'s ledger clear failed
    /// nothing. This pin drives the gated refresh after teardown: the row
    /// must come back, which only happens when the clear actually removed
    /// the entry.
    /// (517/Community suite port: refresh consent leg via
    /// `refresh_gate_consent_sync`, see the C1 test above.)
    #[test]
    fn teardown_ledger_clear_re_arms_the_gated_startup_refresh() {
        use crate::features::marketplace::scope::remove_bundle_from_disabled_scopes_exact;
        use crate::features::marketplace::{
            ConnectorScope, load_disabled_bundles_for, save_disabled_bundles_for,
            sync_deny_all_scopes_after_install,
        };
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-ledger-teardown", || {
            seed_initialized_plain_scope();
            let path = crate::platform::paths::pinvou3_home().join("disabled_bundles.json");

            // Install/connect sync: the row and the ledger entry land.
            sync_deny_all_scopes_after_install("connector-x").unwrap();

            // User enable: the row drops, the ledger entry must survive.
            save_disabled_bundles_for(ConnectorScope::Plain, &[]).unwrap();
            let raw: serde_json::Value =
                serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
            assert!(
                raw["install_default_synced"]
                    .as_array()
                    .map(|entries| entries.iter().any(|entry| entry == "plain:connector-x"))
                    .unwrap_or(false),
                "fixture: the enable must keep the ledger entry"
            );

            // Teardown: the exact removal clears the pack's ledger rows…
            remove_bundle_from_disabled_scopes_exact("connector-x").unwrap();
            let raw: serde_json::Value =
                serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
            assert!(
                raw["install_default_synced"]
                    .as_array()
                    .map(|entries| entries.is_empty())
                    .unwrap_or(false),
                "the teardown must clear the pack's ledger entries"
            );

            // …which re-arms the GATED startup refresh for the same pack (a
            // fresh install seen at boot before the connect sync runs).
            super::refresh_gate_consent_sync("connector-x", "测试连接器").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "after teardown the startup refresh must re-sync default-off"
            );
        });
    }
}
