//! Real connector / marketplace / credential composition regressions.
//! The app owns integration; feature-local unit tests stay with their owner.

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
            crate::features::connectors::skill_gate::refresh_gate_consent_sync(
                "connector-x",
                "测试连接器",
            )
            .unwrap();
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

mod skill_gate_tests {
    /// Exercise the PRODUCTION trait default, not only its injected sequence.
    /// A shortcut in apply_with_consent must not bypass real scope persistence.
    #[test]
    fn production_skill_gate_refusal_never_materializes() {
        use crate::features::connectors::skill_gate::{ConnectorSkillGate, SkillApplyError};
        use crate::features::marketplace::scope::{
            break_scope_lock_for_test, seed_initialized_code_scope_for_test,
        };
        use std::cell::Cell;
        struct Gate(Cell<usize>);
        impl ConnectorSkillGate for Gate {
            fn id(&self) -> &'static str {
                "feishu"
            }
            fn disabled_filename(&self) -> &'static str {
                "feishu_disabled"
            }
            fn apply_skills(&self, _: bool) -> Result<(), String> {
                self.0.set(self.0.get() + 1);
                Ok(())
            }
        }
        crate::platform::test_support::with_temp_home("pinvou3-production-skillgate", || {
            seed_initialized_code_scope_for_test(&["seed-bundle"]);
            break_scope_lock_for_test();
            let gate = Gate(Cell::new(0));
            let error = gate.apply_with_consent(true).unwrap_err();
            assert!(
                matches!(&error, SkillApplyError::Consent(message) if message.contains("disabled_bundles.lock"))
            );
            assert!(
                error
                    .map_materialization(|_| panic!("consent error must stay raw"))
                    .contains("disabled_bundles.lock")
            );
            assert_eq!(
                gate.0.get(),
                0,
                "refusal must happen before any materialization"
            );
        });
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

    /// Round-33 MAJOR 2 (review #455): the connect-path consent-persist
    /// failure copy emitted by the production helper carries the ONE shared frontend marker
    /// (`scope::CONSENT_SYNC_FAILURE_MARKER`) — the localized template on the
    /// store cards keys on exactly this string, so a rewording here must move
    /// the frontend matcher in the same commit. The literal is asserted (not
    /// reformatted and re-matched) so a marker rename fails here instead of
    /// silently degrading the frontend guidance to generic copy.
    /// (517/Community suite port: design-agnostic, kept verbatim — the
    /// frontend pin `consent_marker_frontend.test.mjs` leans on this test.)
    #[test]
    fn skill_gate_consent_failure_message_keeps_the_frontend_marker() {
        use crate::features::marketplace::scope::CONSENT_SYNC_FAILURE_MARKER;

        crate::platform::test_support::with_temp_home("pinvou3-skillgate-consent-marker", || {
            let _ = crate::features::marketplace::load_disabled_bundles_for(
                crate::features::marketplace::ConnectorScope::Plain,
            );
            let _failpoint =
                crate::features::marketplace::scope::fail_next_disabled_bundles_write_for_test();
            let error = crate::features::connectors::skill_gate::apply_skills_consent_sync("wecom")
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
            crate::features::connectors::skill_gate::refresh_gate_consent_sync("feishu", "飞书")
                .unwrap();
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
            crate::features::connectors::skill_gate::refresh_gate_consent_sync("feishu", "飞书")
                .unwrap();
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

            crate::features::connectors::skill_gate::refresh_gate_consent_sync(
                "connector-x",
                "测试连接器",
            )
            .unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the startup refresh must backfill the install-default row for a never-synced pair"
            );

            // The ledger entry must be recorded by that same refresh: a user
            // enable right after the backfill must survive the next refresh.
            save_disabled_bundles_for(ConnectorScope::Plain, &[]).unwrap();
            crate::features::connectors::skill_gate::refresh_gate_consent_sync(
                "connector-x",
                "测试连接器",
            )
            .unwrap();
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
            crate::features::connectors::skill_gate::refresh_gate_consent_sync(
                "connector-x",
                "测试连接器",
            )
            .unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "after teardown the startup refresh must re-sync default-off"
            );
        });
    }
}

mod ima {
    use crate::features::connectors::ima::testing::*;
    use crate::features::marketplace::skill_marketplace::SkillMarketplaceManager;
    use crate::platform::credential_store::{CredentialStore, MemoryCredentialStore};

    /// Temp-home harness for the connect regressions. Delegates to the
    /// shared RAII helper (see its doc for the unwind-restore rationale).
    fn with_temp_home<F: FnOnce()>(f: F) {
        crate::platform::test_support::with_temp_home("pinvou3-ima-connect", f);
    }

    /// Initialize the code scope with the given deny ids while the scope lock
    /// still works (the DenyAll sync then requires a real write). Routes onto
    /// the canonical seed helper in scope.rs.
    fn init_code_scope(ids: &[&str]) {
        crate::features::marketplace::scope::seed_initialized_code_scope_for_test(ids);
    }

    /// Final semantics for a FRESH ima connect (#517 deny-first): the
    /// consent-gate registration is refused when the scope lock is
    /// unavailable, and the refusal must abort BEFORE the credentials are
    /// touched or any skill content lands.
    #[test]
    fn connect_refused_sync_aborts_before_anything_lands() {
        with_temp_home(|| {
            let store = MemoryCredentialStore::default();

            // Initialize the code scope (makes the DenyAll sync a required
            // write), then make the lock file unopenable so the sync refuses.
            init_code_scope(&["seed-bundle"]);
            crate::features::marketplace::scope::break_scope_lock_for_test();

            let error =
                ima_connect_sync_with_store("client-v2".to_string(), "key-v2".to_string(), &store)
                    .unwrap_err();
            assert!(
                error.contains("disabled_bundles.lock"),
                "refusal must name the lock failure: {error}"
            );
            assert!(
                store.get(&client_id_ref()).unwrap().is_none(),
                "credentials must never be written on a refused connect"
            );
            assert!(
                store.get(&api_key_ref()).unwrap().is_none(),
                "credentials must never be written on a refused connect"
            );
            assert!(
                SkillMarketplaceManager::new()
                    .find_skill_dir(IMA_SKILL_ID)
                    .is_none(),
                "the skill must never land on a refused connect"
            );
        });
    }

    /// A RE-connect of
    /// the already-installed ima skill is a known-bundle operation — the
    /// consent gate skips the write entirely (even with the scope lock
    /// unavailable), so the reconnect proceeds and the recorded consent
    /// state stays untouched. The old unconditional re-registration would
    /// have re-denied the enabled skill, and a post-gate failure then left
    /// it disabled with no recovery path.
    #[test]
    fn ima_reconnect_of_installed_skill_preserves_consent_state() {
        with_temp_home(|| {
            let store = MemoryCredentialStore::default();
            store.set(&client_id_ref(), "client-v1").unwrap();
            store.set(&api_key_ref(), "key-v1").unwrap();
            let skills = SkillMarketplaceManager::new();
            skills
                .install(IMA_SKILL_ID)
                .expect("skill install must succeed while the lock works");
            let skill_md = skills
                .find_skill_dir(IMA_SKILL_ID)
                .expect("precondition: ima skill installed")
                .join("SKILL.md");

            // Initialize the code scope (ima stays absent = enabled), then
            // break the lock to prove the reconnect needs no scope write.
            init_code_scope(&["seed-bundle"]);
            crate::features::marketplace::scope::break_scope_lock_for_test();

            ima_connect_sync_with_store("client-v2".to_string(), "key-v2".to_string(), &store)
                .expect("a known bundle's reconnect must not need the consent-gate write");
            assert_eq!(
                store.get(&client_id_ref()).unwrap().as_deref(),
                Some("client-v2"),
                "the reconnect must update the credentials"
            );
            assert_eq!(
                store.get(&api_key_ref()).unwrap().as_deref(),
                Some("key-v2"),
                "the reconnect must update the credentials"
            );
            assert!(
                skill_md.is_file(),
                "the reconnected skill must still be on disk"
            );
            assert!(
                !crate::features::marketplace::load_disabled_bundles_for(
                    crate::features::marketplace::ConnectorScope::Code
                )
                .iter()
                .any(|id| id == "ima"),
                "the reconnect must not re-deny the enabled skill"
            );
        });
    }

    /// A logout must keep the deny entries whenever the uninstall does not
    /// actually tear down an installed skill — both arms:
    /// (1) a VACUOUS uninstall (the skill was never installed): keeps them
    /// per the vacuous-`Ok(false)` contract on
    /// `SkillMarketplaceManager::uninstall`;
    /// (2) a FAILED uninstall (here: the bundle store is unreadable, which
    /// `SkillMarketplaceManager::uninstall` refuses fail-closed): the
    /// on-disk skill (if any) would otherwise be silently re-enabled in
    /// initialized DenyAll scopes. The injected-store core makes both arms
    /// reachable without touching the real system credential vault.
    #[test]
    fn ima_logout_vacuous_and_failed_uninstall_keeps_deny_entries() {
        with_temp_home(|| {
            // Phase 1: vacuous uninstall.
            init_code_scope(&[IMA_SKILL_ID]);

            let store = MemoryCredentialStore::default();
            ima_logout_sync_with_store(&store).expect("a vacuous logout must succeed");

            assert!(
                crate::features::marketplace::scope::load_disabled_bundles_for(
                    crate::features::marketplace::ConnectorScope::Code
                )
                .contains(&"ima".to_string()),
                "a vacuous logout must not strip the deny-first entry"
            );

            // Phase 2: failed uninstall — make the bundle store unreadable
            // (bundles.json becomes a directory, so the uninstall's
            // fail-closed read refuses).
            let bundles_json = crate::features::marketplace::store::BundleStore::new().file_path();
            if bundles_json.is_file() {
                std::fs::remove_file(&bundles_json).unwrap();
            }
            std::fs::create_dir_all(&bundles_json).unwrap();

            ima_logout_sync_with_store(&store)
                .expect("logout must still succeed when only the skill teardown fails");

            assert!(
                crate::features::marketplace::scope::load_disabled_bundles_for(
                    crate::features::marketplace::ConnectorScope::Code
                )
                .contains(&"ima".to_string()),
                "a failed uninstall must keep the deny entries (fail-closed)"
            );
        });
    }

    /// Happy-path end-state pin for a FRESH connect on a working lock:
    /// the deny-first entry lands in the initialized Code scope
    /// ("ima-skills" normalizes to its owner-package id "ima" on save) and
    /// the credentials round-trip.
    #[test]
    fn fresh_connect_lands_deny_entry_and_credentials() {
        with_temp_home(|| {
            init_code_scope(&["seed-bundle"]);

            let store = MemoryCredentialStore::default();
            ima_connect_sync_with_store("client".to_string(), "key".to_string(), &store)
                .expect("a fresh connect must succeed while the lock works");

            assert!(
                crate::features::marketplace::scope::load_disabled_bundles_for(
                    crate::features::marketplace::ConnectorScope::Code,
                )
                .contains(&"ima".to_string()),
                "the fresh connect must register the deny-first entry"
            );
            assert_eq!(
                store.get(&client_id_ref()).unwrap().as_deref(),
                Some("client"),
                "the connect must write the credentials"
            );
            assert_eq!(
                store.get(&api_key_ref()).unwrap().as_deref(),
                Some("key"),
                "the connect must write the credentials"
            );
        });
    }
}
