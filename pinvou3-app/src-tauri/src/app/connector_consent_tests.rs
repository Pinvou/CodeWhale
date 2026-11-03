//! Real connector / marketplace / credential composition regressions.
//! The app owns integration; feature-local unit tests stay with their owner.

mod consent_sync_tests {
    use crate::features::marketplace::{ConnectorScope, load_disabled_bundles_for};
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

    /// Round-31 BLOCKER negative control (review #455), flipped to the grant
    /// direction (feat/connector-connect-grants-enable), consent-leg form: an
    /// initialized scope, a connected connector the user explicitly turned OFF
    /// after its connect grant, then the boot refresh's consent leg — the
    /// stored row must survive. This is the test that fails on the round-30
    /// form (a plain membership push re-added the row at every boot); under
    /// the grant it is the re-GRANT that must stay ledger-gated: a refresh
    /// re-granting would REMOVE the user's row. Also pins the reconnect
    /// direction — teardown (exact removal) clears the ledger entry AND the
    /// grant, so a fresh install / reconnect re-grants.
    /// (517/Community suite port: the consent leg drives
    /// `grant_connect_enable_refresh` — the Official trait design's boot
    /// consent leg, where the verdict guard lives.)
    #[test]
    fn refresh_consent_leg_does_not_revert_user_off_but_reconnect_regrants() {
        use crate::features::marketplace::scope::{
            grant_connect_enable, remove_bundle_from_disabled_scopes_exact,
        };

        with_temp_home("pinvou3-skillgate-consent-sync", || {
            // Initialize plain via the raw store shape (the migration's own
            // verdict write; the composer's first write seeds the same).
            seed_initialized_plain_scope();

            // First connect: the grant lands — the connector is ENABLED (no
            // stored row) and the pair is ledgered.
            grant_connect_enable("connector-x").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the connect grant must leave the connector enabled"
            );

            // The user turns the connector off: the composer whole-list write
            // adds the row and removes the grant, while the ledger entry
            // survives.
            crate::features::marketplace::save_disabled_bundles_for(
                ConnectorScope::Plain,
                &["connector-x".to_string()],
            )
            .unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "fixture: the user off added the row"
            );

            // The startup refresh's consent leg runs for the connected
            // connector: the LEDGER must keep it from re-granting — a
            // re-grant would delete the user's row (the round-30/31 family
            // failure in the grant direction).
            crate::features::marketplace::grant_connect_enable_refresh("connector-x").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the startup refresh must NOT revert an explicit user off (a ledger-gate loss re-grants here)"
            );

            // Reconnect direction: teardown (exact removal) clears the
            // ledger entry and the grant, so the next consent write re-grants
            // the connector enabled.
            remove_bundle_from_disabled_scopes_exact("connector-x").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "fixture: the teardown cleared the stored row"
            );
            grant_connect_enable("connector-x").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "a fresh install / reconnect must re-grant the connector enabled after teardown"
            );
        });
    }
}

mod skill_gate_tests {
    /// Exercise the PRODUCTION trait default, not only its injected sequence.
    /// A shortcut in apply_with_consent must not bypass real scope persistence.
    /// (feat/connector-connect-grants-enable: the apply command's gate is the
    /// GESTURE variant, so its persist refusal is wrapped by the connect
    /// channel's fail-closed consent copy — the raw lock cause stays in the
    /// tail, and the frontend connect marker must ride the message.)
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
            let message = error.map_materialization(|_| panic!("consent error must stay raw"));
            assert!(
                message.contains("disabled_bundles.lock"),
                "the raw lock cause stays in the wrapped copy: {message}"
            );
            assert!(
                message.contains(
                    crate::features::marketplace::scope::CONNECT_CONSENT_SYNC_FAILURE_MARKER
                ),
                "the gesture gate's persist refusal must carry the connect consent marker: {message}"
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

    /// Round-33 MAJOR 2 (review #455), dual-marker form
    /// (feat/connector-connect-grants-enable): the connect channel's
    /// consent-persist failure copy carries the ONE shared connect marker
    /// (`scope::CONNECT_CONSENT_SYNC_FAILURE_MARKER`) — the localized template
    /// on the store cards keys on exactly this string, so a rewording here must
    /// move the frontend matcher in the same commit. The install channels keep
    /// `CONSENT_SYNC_FAILURE_MARKER` (their failure direction is unchanged).
    /// The literals are asserted (not reformatted and re-matched) so a marker
    /// rename fails here instead of silently degrading the frontend guidance
    /// to generic copy.
    /// (517/Community suite port: the frontend pin
    /// `consent_marker_frontend.test.mjs` leans on this test.)
    #[test]
    fn skill_gate_consent_failure_message_keeps_the_frontend_marker() {
        use crate::features::marketplace::scope::{
            CONNECT_CONSENT_SYNC_FAILURE_MARKER, CONNECT_GRANT_COMMIT_FAILURE_MARKER,
            CONSENT_SYNC_FAILURE_MARKER,
        };

        assert_eq!(
            CONNECT_CONSENT_SYNC_FAILURE_MARKER,
            "persisting the connector's connect-enabled consent failed",
            "the shipped connect marker is the string the frontend consentFailure matcher keys on"
        );
        assert_eq!(
            CONSENT_SYNC_FAILURE_MARKER, "persisting their default-off consent state failed",
            "the install channels keep the original marker"
        );
        assert_eq!(
            CONNECT_GRANT_COMMIT_FAILURE_MARKER,
            "finalizing the connector's connect-enabled consent failed",
            "the commit-stage marker is the string the frontend matcher keys on (round-9 review P2: the commit-persist failure leaves the connector enabled, so it must not reuse the fail-closed copy)"
        );
    }

    /// Round-37 C1 (review #455), grant form
    /// (feat/connector-connect-grants-enable): a consent persist failure
    /// between connect and enable left the pair unledgered, so the startup
    /// refresh reverted the user's enable across the restart (round-30 family
    /// residue). The explicit-enable path now RECORDS the ledger pair, closing
    /// the window: connect-grant persist fails → nothing lands, the connector
    /// stays off (fail-closed) → the user's enable succeeds → refresh must not
    /// re-grant over it.
    /// (517/Community suite port: the consent legs drive the grant writers via
    /// the crate-internal marketplace surface — the Official trait design's
    /// equivalent of upstream `ConnectorGate::refresh_step`'s ledger-gated
    /// grant leg.)
    #[test]
    fn explicit_enable_records_the_ledger_over_a_failed_connect_sync() {
        use crate::features::marketplace::scope::enable_packages_in_scope;
        use crate::features::marketplace::{ConnectorScope, load_disabled_bundles_for};
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-ledger-failedconnect", || {
            // Pre-warm: the first read persists the freeze verdict, settling
            // that write so the failpoint below hits the GRANT's save.
            let _ = crate::features::marketplace::scope::load_disabled_bundles_for(
                ConnectorScope::Plain,
            );
            // The connect-path grant's persist FAILS: neither the grant nor
            // the ledger entry reaches disk (and the deny-first caller
            // materializes nothing — fail-closed).
            let _failpoint =
                crate::features::marketplace::scope::fail_next_disabled_bundles_write_for_test();
            assert!(
                crate::features::marketplace::grant_connect_enable("feishu").is_err(),
                "fixture: the connect grant must fail on the injected persist error"
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

            // The enable must have RECORDED THE LEDGER — pinned on the raw
            // store, because in the grant world a re-grant over an
            // already-enabled connector is stored-list-invisible and the
            // final refresh assert below cannot see the ledger write (the
            // defanged-pin finding).
            {
                let raw: serde_json::Value = serde_json::from_str(
                    &std::fs::read_to_string(
                        crate::platform::paths::pinvou3_home().join("disabled_bundles.json"),
                    )
                    .unwrap(),
                )
                .unwrap();
                let ledger = raw["install_default_synced"].as_array().unwrap();
                assert!(
                    ledger.contains(&serde_json::json!("plain:feishu")),
                    "the enable must record the ledger pair for the scope it enabled (the any-mode gate needs one pair to stay suppressed), or the next boot re-grants over it: {raw}"
                );
            }

            // The startup refresh must stay gated by the ledger the ENABLE
            // wrote — no re-grant over the user's state.
            crate::features::marketplace::grant_connect_enable_refresh("feishu").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the enable must record the ledger, or the next boot re-grants over it (round-30 family residue)"
            );
        });
    }

    /// Round-32 MAJOR 1 (review #455) twin negative control, grant form
    /// (feat/connector-connect-grants-enable): the connect happens while plain
    /// is STILL UNINITIALIZED (fresh home) — under the grant the connector is
    /// ENABLED immediately (the DenyAll expansion subtracts `connect_granted`),
    /// and it must still be enabled after the user's first composer write
    /// materializes the scope and the startup refresh runs. This fails on the
    /// round-31 form in BOTH directions: a default-off registration would deny
    /// the connector up front, and an ungated refresh would backfill over the
    /// recorded grant. The uninitialized arm's ledger record is what keeps the
    /// post-materialization refresh from re-denying.
    /// (517/Community suite port: consent legs via the grant writers, see the
    /// C1 test above.)
    #[test]
    fn enable_after_uninitialized_connect_survives_startup_refresh() {
        use crate::features::marketplace::scope::enable_packages_in_scope;
        use crate::features::marketplace::{ConnectorScope, load_disabled_bundles_for};
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-ledger-uninit", || {
            // Fresh home: plain does not exist yet. The connector connects
            // FIRST (the connect-path grant — a user action) while plain is
            // uninitialized by design.
            crate::features::marketplace::grant_connect_enable("feishu").unwrap();

            // THE fix: the connector is enabled in the computed default right
            // away — the user does not need to find any toggle before their
            // first lark call works.
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the connect grant must enable the connector in the uninitialized scope's DenyAll expansion"
            );

            // The ledger pair must be recorded by that same uninitialized
            // grant — pinned on the raw store, because a re-grant over an
            // already-enabled connector is stored-list-invisible: without
            // this leg, deleting the ledger write kept every later assertion
            // green (the defanged-pin finding).
            {
                let raw: serde_json::Value = serde_json::from_str(
                    &std::fs::read_to_string(
                        crate::platform::paths::pinvou3_home().join("disabled_bundles.json"),
                    )
                    .unwrap(),
                )
                .unwrap();
                let ledger = raw["install_default_synced"].as_array().unwrap();
                assert!(
                    ledger.contains(&serde_json::json!("plain:feishu"))
                        && ledger.contains(&serde_json::json!("code:feishu")),
                    "the uninitialized connect grant must record both scope ledger pairs: {raw}"
                );
            }

            // The user's first composer write materializes the scope (the
            // whole effective list, where feishu renders ON and wecom is the
            // pack the user actually opted into): the stored snapshot excludes
            // feishu both because the grant subtracted it from the expansion
            // and because the composer did not list it.
            let outcome =
                enable_packages_in_scope(ConnectorScope::Plain, &["wecom".to_string()]).unwrap();
            assert!(
                outcome.state_changed,
                "fixture: the first write must materialize the scope"
            );

            // Restart equivalent: the startup refresh runs for the connected
            // gate — the ledger entry recorded by the uninitialized connect
            // grant must keep the connector enabled.
            crate::features::marketplace::grant_connect_enable_refresh("feishu").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the startup refresh must not re-deny a connector granted on an uninitialized scope (round-32 MAJOR 1)"
            );
        });
    }

    /// Round-32 minor 2 (review #455), grant form
    /// (feat/connector-connect-grants-enable): the refresh's consent leg
    /// itself is load-bearing and had no pin — a regression turning the
    /// refresh into a no-op kept the suite green while silently disabling the
    /// round-30 m1 backfill. The refresh must GRANT the connector (enabled, no
    /// default-off row) for a pair no consent write ever recorded, AND record
    /// the ledger entry while doing it. (Round-1 review note: the closing
    /// toggle-off-stays-off leg is guarded by the in-lock VERDICT GUARD — the
    /// stored row blocks the refresh regardless of the ledger — so the ledger
    /// recording is pinned on the RAW store in the first half; the raw pin is
    /// what a ledger-write deletion cannot survive.)
    /// (517/Community suite port: refresh consent leg via
    /// `grant_connect_enable_refresh`, see the C1 test above.)
    #[test]
    fn refresh_backfills_never_synced_row_and_records_the_ledger() {
        use crate::features::marketplace::{
            ConnectorScope, load_disabled_bundles_for, save_disabled_bundles_for,
        };
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-ledger-backfill", || {
            // Initialized plain (post-migration shape), connector-x never
            // seen by any consent write: no stored row, no ledger entry, no
            // grant.
            seed_initialized_plain_scope();

            crate::features::marketplace::grant_connect_enable_refresh("connector-x").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the startup refresh must GRANT the never-synced connector (enabled, not default-off)"
            );
            {
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
                        .map(|ids| ids.iter().any(|id| id == "connector-x"))
                        .unwrap_or(false),
                    "the backfill must record the grant: {raw}"
                );
            }

            // The ledger entry must be recorded by that same refresh: a user
            // toggle-off right after the backfill must survive the next
            // refresh.
            save_disabled_bundles_for(ConnectorScope::Plain, &["connector-x".to_string()]).unwrap();
            crate::features::marketplace::grant_connect_enable_refresh("connector-x").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "the backfill must record the ledger, or the next refresh re-grants over the user off (round-31 BLOCKER re-opened)"
            );
        });
    }

    /// Round-32 minor 2 (review #455): the TEARDOWN clears are what re-arms
    /// the LEDGER-GATED startup refresh, and (feat/connector-connect-grants-
    /// enable) the teardown must sweep the pack's CONNECT GRANT alongside the
    /// ledger. Round-1 review: driving the teardown after a composer
    /// toggle-off would keep every assertion green even with the sweep deleted
    /// (the toggle-off had ALREADY removed the grant — mutation-proven on the
    /// community tree), so the teardown is driven with a LIVE grant.
    /// (517/Community suite port: refresh consent leg via
    /// `grant_connect_enable_refresh`, see the C1 test above.)
    #[test]
    fn teardown_ledger_clear_re_arms_the_gated_startup_refresh() {
        use crate::features::marketplace::scope::remove_bundle_from_disabled_scopes_exact;
        use crate::features::marketplace::{ConnectorScope, load_disabled_bundles_for};
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-ledger-teardown", || {
            seed_initialized_plain_scope();
            let path = crate::platform::paths::pinvou3_home().join("disabled_bundles.json");

            // Connect grant on an initialized scope holding no row: the
            // grant and the ledger entry land (enabled-by-absence).
            crate::features::marketplace::grant_connect_enable("connector-x").unwrap();
            let raw: serde_json::Value =
                serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "connector-x")),
                "fixture: the connect grant must be live before the teardown: {raw}"
            );
            assert!(
                raw["install_default_synced"]
                    .as_array()
                    .is_some_and(|entries| entries
                        .iter()
                        .any(|entry| entry == "plain:connector-x")),
                "fixture: the ledger pair must be live before the teardown: {raw}"
            );

            // Teardown: the exact removal clears the pack's ledger rows…
            remove_bundle_from_disabled_scopes_exact("connector-x").unwrap();
            let raw: serde_json::Value =
                serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
            assert!(
                raw["install_default_synced"]
                    .as_array()
                    .is_some_and(|entries| entries.is_empty()),
                "the teardown must clear the pack's ledger entries: {raw}"
            );
            // …and the connect grant: a leftover grant would pre-enable a
            // future same-id reinstall's DenyAll expansion before its connect
            // flow re-grants. With the grant LIVE at the teardown, deleting
            // the sweep fails exactly here.
            assert_eq!(
                raw["connect_granted"],
                serde_json::json!([]),
                "the teardown must sweep the LIVE connect grant: {raw}"
            );

            // …which re-arms the GATED startup refresh for the same pack (a
            // fresh install seen at boot before the connect grant runs): the
            // connector comes back ENABLED via the grant, not default-off.
            crate::features::marketplace::grant_connect_enable_refresh("connector-x").unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .contains(&"connector-x".to_string()),
                "after teardown the startup refresh must re-grant the connector enabled"
            );
            let raw: serde_json::Value =
                serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "connector-x")),
                "the re-armed refresh must re-grant the pack: {raw}"
            );
        });
    }

    /// Review BLOCKER regression pin (feat/connector-connect-grants-enable),
    /// consent-leg form: a connect whose grant persist FAILED (fail-closed,
    /// nothing ledgered), then a composer toggle-off (the composer writer
    /// never records the ledger) — the next boot's refresh consent leg probes
    /// the connector with NO ledger entry, and an ungated gesture write would
    /// delete the user's row and silently flip the connector ON. The refresh
    /// must drive the verdict-respecting variant: the row, the empty grant
    /// set, and the empty ledger all survive.
    /// (517/Community suite port: upstream drives the whole `refresh_step`
    /// body; the Official trait design splits the boot consent leg into
    /// `grant_connect_enable_refresh`, which is exactly the leg the verdict
    /// guard lives in.)
    #[test]
    fn boot_refresh_never_reverts_a_verdict_after_a_failed_connect() {
        use crate::features::marketplace::{ConnectorScope, load_disabled_bundles_for};
        use crate::platform::test_support::with_temp_home;

        with_temp_home("pinvou3-skillgate-bootverdict", || {
            seed_initialized_plain_scope();
            // Warm the freeze verdict so the failpoint below hits the GRANT's
            // save (same pre-warm as the explicit-enable pin).
            let _ = load_disabled_bundles_for(ConnectorScope::Plain);
            let _failpoint =
                crate::features::marketplace::scope::fail_next_disabled_bundles_write_for_test();
            assert!(
                crate::features::marketplace::grant_connect_enable("feishu").is_err(),
                "fixture: the connect grant must fail on the injected persist error"
            );

            // The user turns the connector off: the row lands, nothing is
            // ledgered, nothing is granted.
            crate::features::marketplace::save_disabled_bundles_for(
                ConnectorScope::Plain,
                &["feishu".to_string()],
            )
            .unwrap();
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
                "fixture: the failed connect left no grant: {raw}"
            );
            assert!(
                !raw["install_default_synced"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|entry| entry == "plain:feishu"),
                "fixture: no ledger entry may exist — this is the guard's blind-spot cohort: {raw}"
            );

            // The boot: show=true (connected, no disable flag), no ledger —
            // the refresh must not lift the verdict.
            crate::features::marketplace::grant_connect_enable_refresh("feishu").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the boot refresh must not revert the user's post-failure off"
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
                "the boot refresh must not grant over the verdict: {raw}"
            );
            assert!(
                !raw["install_default_synced"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the boot refresh must not ledger over the verdict: {raw}"
            );
        });
    }
}

/// The compensating-revoke / relied-grant-commit contract pins
/// (feat/connector-connect-grants-enable, community rounds 1-12). On the
/// community tree these live against the monolithic `apply_skills_command`
/// body; the Official trait design splits that body, and every invariant here
/// is expressed at the scope layer it actually protects — the gate outcome
/// comes from the real gated entries (`deny_first_register_connector` /
/// `deny_first_register_connector_refresh`), and the compensating revoke /
/// commit run through the same crate-internal surface the production bodies
/// drive.
mod grant_revoke_commit_tests {
    use crate::features::marketplace::{
        ConnectorScope, deny_first_register_connector, deny_first_register_connector_refresh,
        load_disabled_bundles_for, revoke_connect_enable, save_disabled_bundles_for,
    };
    use crate::platform::test_support::with_temp_home;

    fn store_json() -> serde_json::Value {
        serde_json::from_str(
            &std::fs::read_to_string(
                crate::platform::paths::pinvou3_home().join("disabled_bundles.json"),
            )
            .unwrap(),
        )
        .unwrap()
    }

    /// Round-2 review: the exact-footprint restore has no pin of its own —
    /// the sibling pins lift rows in EVERY initialized scope, where the
    /// footprinted and the unconditional restore are indistinguishable. This
    /// pin builds the discriminating shape: plain initialized holding the
    /// default-off row (the grant lifts it), code initialized EMPTY (the
    /// grant lifts nothing there) — the compensation must rebuild plain's row
    /// and NOT downgrade code's enabled-by-absence to a liftable default-off
    /// row. Reverting the revoke to the unconditional per-mode restore fails
    /// the code-scope legs. Round-3 review: the fixture row is an UNMARKED
    /// no-ledger verdict, so the restore must also keep its attribution —
    /// re-adding a registration marker the pre-grant state did not have
    /// would downgrade the explicit verdict to a liftable install default.
    #[test]
    fn revoke_restores_exactly_the_scopes_the_grant_lifted() {
        with_temp_home("pinvou3-skillgate-revoke-footprint", || {
            let store = crate::platform::paths::pinvou3_home().join("disabled_bundles.json");
            std::fs::write(
                &store,
                r#"{"scopes":{"plain":["feishu"]},"initialized":["plain","code"],"plain_defaults_migrated":true}"#,
            )
            .unwrap();
            // Fixture check: code's stored list is empty, so feishu is
            // enabled-by-absence there before the connect.
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Code).contains(&"feishu".to_string()),
                "fixture: code must compute feishu ON by row absence"
            );

            // The gesture grant (what the connect body's gate writes) lifts
            // plain's row; the "materialization failure" is this test driving
            // the compensating revoke directly, exactly what the M4 hook runs.
            let grant = deny_first_register_connector("feishu", true).unwrap();
            assert!(
                grant.granted && grant.lifted_scopes.iter().any(|k| k == "plain"),
                "fixture: the gesture grants and lifts plain's row: {grant:?}"
            );
            revoke_connect_enable(&grant).unwrap();
            let raw = store_json();
            assert_eq!(
                raw["scopes"]["plain"],
                serde_json::json!(["feishu"]),
                "the lifted row must be restored in the scope the grant lifted: {raw}"
            );
            assert!(
                raw["default_off_scopes"].get("plain").is_none_or(|ids| !ids
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu"))),
                "the restored row must keep its pre-grant attribution — the fixture row is an \
                 UNMARKED no-ledger verdict, and re-adding a registration marker would \
                 downgrade it to a liftable install default (round-3 review): {raw}"
            );
            // THE footprint legs: the grant lifted nothing in code, so the
            // compensation must leave code's stored shape alone — no row, no
            // marker, feishu still computed-ON there.
            assert!(
                !raw["scopes"].get("code").is_some_and(|ids| ids
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu"))),
                "the compensation must not downgrade code's explicit on: {raw}"
            );
            assert!(
                !raw["default_off_scopes"].get("code").is_some_and(|ids| ids
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu"))),
                "the compensation must not marker code's enabled-by-absence: {raw}"
            );
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Code).contains(&"feishu".to_string()),
                "code must stay computed-ON after the compensation"
            );
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_none_or(|ids| ids.iter().all(|id| id != "feishu"))
                    && !raw["install_default_synced"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the revoke must still strip the grant and the ledger: {raw}"
            );

            // The restored plain row is verdict-shaped: the boot refresh must
            // not complete the enable over it.
            crate::features::marketplace::grant_connect_enable_refresh("feishu").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the boot refresh must not complete the enable over the restored verdict"
            );
        });
    }

    /// Round-2 review M1: the gesture grant lifts stored rows in EVERY
    /// initialized DenyAll scope, and an initialized scope's stored list is
    /// its sole truth — so a connect whose materialization then failed used
    /// to leave the connector computed-ON by row-absence behind the failure
    /// card, with the next boot refresh silently completing the enable. The
    /// revoke must REBUILD the lifted rows — row back in every initialized
    /// DenyAll scope, so the failure end-state is fail-closed on both scope
    /// kinds and the restored row re-arms the verdict guard against the boot
    /// backfill. Round-3 review: the restoration is attribution-faithful — an
    /// UNMARKED verdict row must come back UNMARKED.
    #[test]
    fn revoke_restores_the_lifted_rows_in_initialized_scopes() {
        with_temp_home("pinvou3-skillgate-revoke-restore", || {
            // Initialized plain scope holding an off-row, no ledger, no
            // grant: the no-ledger verdict cohorts (a legacy-migrated
            // disable, or a toggle-off after a failed connect persist — the
            // exact state the failure card's "reconnect to retry" steers
            // into).
            let store = crate::platform::paths::pinvou3_home().join("disabled_bundles.json");
            std::fs::write(
                &store,
                r#"{"scopes":{"plain":["feishu"]},"initialized":["plain"],"plain_defaults_migrated":true}"#,
            )
            .unwrap();

            let grant = deny_first_register_connector("feishu", true).unwrap();
            assert!(
                grant.granted && grant.lifted_scopes.iter().any(|k| k == "plain"),
                "fixture: the gesture lifts the stale row: {grant:?}"
            );
            revoke_connect_enable(&grant).unwrap();
            let raw = store_json();
            assert_eq!(
                raw["scopes"]["plain"],
                serde_json::json!(["feishu"]),
                "the revoke must restore the lifted row in the initialized scope: {raw}"
            );
            assert!(
                raw["default_off_scopes"].get("plain").is_none_or(|ids| !ids
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu"))),
                "the revoke must restore the verdict's own attribution — the fixture row is \
                 UNMARKED, and re-adding a registration marker would downgrade it to a \
                 liftable install default (round-3 review): {raw}"
            );
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_none_or(|ids| ids.iter().all(|id| id != "feishu"))
                    && !raw["install_default_synced"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the revoke must still remove the grant and the ledger: {raw}"
            );
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the initialized scope must compute OFF again after the revoke"
            );

            // The restored row is verdict-shaped: the boot refresh must not
            // complete the enable the failed connect started.
            crate::features::marketplace::grant_connect_enable_refresh("feishu").unwrap();
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain).contains(&"feishu".to_string()),
                "the boot refresh must not complete the enable over the restored verdict"
            );
            let raw = store_json();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_none_or(|ids| ids.iter().all(|id| id != "feishu")),
                "the boot refresh must not grant over the restored verdict: {raw}"
            );
        });
    }

    /// Round-3 review MAJOR: the fresh-home grant covers the UNINITIALIZED
    /// DenyAll scopes through the grant alone — `lifted_scopes` stays empty,
    /// so the lifted-row restores cannot see them. If such a scope
    /// MATERIALIZES between the grant and the compensating revoke (the
    /// composer's whole-list write or another pack's enable initializing it
    /// from a post-grant snapshot names other packs and keeps the grant), its
    /// stored list computes the connector ON by row-absence once the grant is
    /// gone: fail-open behind the failure card, and the next boot refresh
    /// completes the enable (no row anywhere for the verdict guard, no ledger
    /// for the gate). The revoke must rebuild the deny-first shape (row +
    /// marker — what a deny-first registration would have written) in the
    /// covered scopes that have since materialized.
    #[test]
    fn revoke_restores_a_scope_that_materializes_mid_connect() {
        with_temp_home("pinvou3-skillgate-revoke-covered", || {
            // Fresh home: NEITHER DenyAll scope is initialized, so the grant
            // lands in the uninitialized arms — grant + ledger pairs, no
            // rows, both scopes covered on the outcome.
            let grant = deny_first_register_connector("feishu", true).unwrap();
            assert!(grant.granted, "fixture: the fresh-home grant must land");
            assert!(
                grant.lifted_scopes.is_empty(),
                "fixture: an uninitialized scope lifts nothing: {grant:?}"
            );
            assert_eq!(
                grant.covered_uninitialized_scopes.len(),
                2,
                "fixture: both DenyAll scopes are covered uninitialized: {grant:?}"
            );

            // Mid-connect: an unrelated composer write materializes plain
            // from the post-grant snapshot (feishu renders ON, so it is not
            // in the stored list and its grant survives the write).
            save_disabled_bundles_for(ConnectorScope::Plain, &["dingtalk".to_string()]).unwrap();
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .iter()
                    .any(|id| id == "feishu"),
                "fixture: plain computes feishu ON by row absence behind the grant"
            );

            revoke_connect_enable(&grant).unwrap();
            let raw = store_json();
            assert!(
                raw["scopes"]["plain"]
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the materialized covered scope must get its deny-first row back: {raw}"
            );
            assert!(
                raw["default_off_scopes"]["plain"]
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the restored deny-first shape carries the registration marker: {raw}"
            );
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_none_or(|ids| ids.iter().all(|id| id != "feishu"))
                    && !raw["install_default_synced"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the revoke must still strip the grant and the ledger: {raw}"
            );
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .iter()
                    .any(|id| id == "feishu"),
                "the restored row must compute the connector OFF again"
            );
            assert!(
                load_disabled_bundles_for(ConnectorScope::Code)
                    .iter()
                    .any(|id| id == "feishu"),
                "the still-uninitialized covered scope denies via the expansion again"
            );

            // The restored plain row re-arms the verdict guard: a boot
            // refresh must not complete the enable over it.
            let regrant =
                crate::features::marketplace::grant_connect_enable_refresh("feishu").unwrap();
            assert!(
                !regrant.granted && !regrant.relied_standing_consent,
                "the boot refresh must not grant over the restored verdict"
            );
        });
    }

    /// Round-6 review P2: the compensating revoke owns its grant by the
    /// operation token, not the package id. A teardown + reconnect between
    /// the grant and its compensating revoke lands a fresh grant under the
    /// same package id; the stale failure arriving late must leave the
    /// replacement's consent state (grant, ledger, lifted rows) completely
    /// alone, while the replacement's OWN revoke still compensates in full.
    #[test]
    fn stale_revoke_leaves_a_replacement_grant_alone() {
        with_temp_home("pinvou3-skillgate-revoke-stale", || {
            // plain initialized holding a MARKED feishu row (the deny-first
            // shape): operation A's grant lifts it.
            save_disabled_bundles_for(ConnectorScope::Plain, &["feishu".to_string()]).unwrap();
            let grant_a = deny_first_register_connector("feishu", true).unwrap();
            assert!(grant_a.granted, "fixture: A's grant lifts the row");
            assert_eq!(grant_a.lifted_scopes, vec!["plain".to_string()]);

            // The pack is uninstalled mid-connect: the exact teardown sweeps
            // A's grant, ledger pair, rows, and ownership token.
            crate::features::marketplace::scope::remove_bundle_from_disabled_scopes_exact("feishu")
                .unwrap();
            // B reconnects: the re-registered deny-first row (plain is
            // initialized by now, so the save writes it UNMARKED) is lifted
            // by a fresh grant under the same package id.
            save_disabled_bundles_for(ConnectorScope::Plain, &["feishu".to_string()]).unwrap();
            let grant_b = deny_first_register_connector("feishu", true).unwrap();
            assert!(grant_b.granted, "fixture: B's reconnect grants afresh");
            assert_ne!(
                grant_a.grant_token, grant_b.grant_token,
                "fixture: distinct operations must own distinct tokens"
            );

            // A's materialization failure arrives late. The stale revoke
            // must keep its hands off B's state entirely.
            revoke_connect_enable(&grant_a).unwrap();
            let raw = store_json();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the stale revoke must not delete the replacement grant: {raw}"
            );
            assert!(
                raw["install_default_synced"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the replacement's ledger pair must survive the stale revoke: {raw}"
            );
            assert!(
                raw["scopes"]["plain"]
                    .as_array()
                    .is_none_or(|ids| ids.iter().all(|id| id != "feishu")),
                "the stale revoke must not restore A's lifted row over B: {raw}"
            );
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .iter()
                    .any(|id| id == "feishu"),
                "B's successful connection must stay enabled"
            );

            // B's own compensating revoke still strips and restores in full:
            // the ownership proof is per operation, not a blanket disarm.
            revoke_connect_enable(&grant_b).unwrap();
            let raw = store_json();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_none_or(|ids| ids.iter().all(|id| id != "feishu"))
                    && !raw["install_default_synced"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the owning revoke strips its own grant and ledger: {raw}"
            );
            assert!(
                raw["scopes"]["plain"]
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the owning revoke restores the deny-first row: {raw}"
            );
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .iter()
                    .any(|id| id == "feishu"),
                "the failed connect's end-state is fail-closed again"
            );
        });
    }

    /// Round-7 review P2: an overlapping completion must not lose its consent
    /// to the older in-flight operation's compensation. A's refresh grants on
    /// a fresh home and materializes (in flight); B's connect gate hits the
    /// ledger short-circuit, relies on the standing consent, and captures A's
    /// token; B completes successfully and commits the relied-upon grant. A's
    /// late materialization failure must then find the standing entry owned
    /// by no pending claim and leave B's consent state (grant + ledger +
    /// computed-ON) untouched.
    #[test]
    fn committed_relied_grant_survives_the_in_flight_revoke() {
        with_temp_home("pinvou3-skillgate-commit-relied", || {
            // Operation A: the refresh grant lands on the fresh home (its
            // materialization is now in flight).
            let grant_a = deny_first_register_connector_refresh("feishu", true).unwrap();
            assert!(grant_a.granted, "fixture: A's refresh grant lands");
            // Operation B overlaps: the gate finds A's ledger and relies on
            // the standing consent, capturing A's token instead of granting.
            let gate_b = deny_first_register_connector("feishu", true).unwrap();
            assert!(
                !gate_b.granted,
                "fixture: B's gate declines a fresh grant over the standing ledger"
            );
            assert_eq!(
                gate_b.relied_grant_token, grant_a.grant_token,
                "B must capture the token of the grant it relies on: {gate_b:?}"
            );
            // B's tail grant is skipped the same way (the ledger stands), so
            // nothing of B's own lands — B succeeds on A's standing state.
            let tail_b = crate::features::marketplace::grant_connect_enable("feishu").unwrap();
            assert!(
                !tail_b.granted && tail_b.relied_standing_consent,
                "fixture: B's tail grant skips over the standing consent"
            );
            assert_eq!(
                tail_b.relied_grant_token, grant_a.grant_token,
                "fixture: the tail observes the same standing token the gate captured"
            );
            // B completes successfully: the relied-upon grant is committed,
            // ending A's pending ownership claim.
            let committed = crate::features::marketplace::commit_relied_connect_grant(
                "feishu",
                &gate_b.relied_grant_token,
            )
            .unwrap();
            assert_eq!(
                committed,
                crate::features::marketplace::ReliedGrantCommit::Committed,
                "the relied-upon grant must commit"
            );

            // A's materialization failure arrives late: its revoke must no
            // longer match and must leave B's consent state alone.
            revoke_connect_enable(&grant_a).unwrap();
            let raw = store_json();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the stale revoke must not delete the committed grant: {raw}"
            );
            assert!(
                raw["install_default_synced"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the ledger pair B's success relied on must survive: {raw}"
            );
            assert!(
                raw["connect_grant_tokens"]["feishu"].is_null(),
                "the commit cleared the pending ownership token: {raw}"
            );
            assert!(
                !load_disabled_bundles_for(ConnectorScope::Plain)
                    .iter()
                    .any(|id| id == "feishu"),
                "the connector must still compute ON after the stale revoke"
            );

            // A retry of the same commit declines as superseded (the grant
            // still stands, its token is gone), and an empty relied token
            // reports the no-reliance no-op.
            assert_eq!(
                crate::features::marketplace::commit_relied_connect_grant(
                    "feishu",
                    &gate_b.relied_grant_token
                )
                .unwrap(),
                crate::features::marketplace::ReliedGrantCommit::Superseded,
                "the commit is once-only: the retry finds no relied token"
            );
            assert_eq!(
                crate::features::marketplace::commit_relied_connect_grant("feishu", "").unwrap(),
                crate::features::marketplace::ReliedGrantCommit::NotRelied,
                "an empty relied token declines without touching the store"
            );
        });
    }

    /// Round-11 review P2: a completion that captured A's token must not
    /// secure (or report) success while the standing grant belongs to a
    /// newer in-flight operation C. The commit reports ReplacementPending
    /// without writing, leaving C's pending compensation armed.
    #[test]
    fn stale_commit_leaves_a_replacement_owner_alone() {
        with_temp_home("pinvou3-skillgate-commit-stale", || {
            let grant_a = deny_first_register_connector_refresh("feishu", true).unwrap();
            assert!(grant_a.granted, "fixture: A's refresh grant lands");
            let gate_b = deny_first_register_connector("feishu", true).unwrap();
            assert_eq!(
                gate_b.relied_grant_token, grant_a.grant_token,
                "fixture: B relies on A's grant"
            );
            // Mid-flight: the teardown sweeps A's consent entirely, then C
            // reconnects and grants afresh under the same package id.
            crate::features::marketplace::scope::remove_bundle_from_disabled_scopes_exact("feishu")
                .unwrap();
            let grant_c = deny_first_register_connector("feishu", true).unwrap();
            assert!(grant_c.granted, "fixture: C's reconnect grants afresh");
            assert_ne!(
                grant_c.grant_token, grant_a.grant_token,
                "fixture: distinct operations must own distinct tokens"
            );
            // B completes and tries to commit — against the token it relied
            // on. The standing entry is C's now, so the commit must report
            // the pending replacement (the caller declines the success) and
            // write nothing.
            let committed = crate::features::marketplace::commit_relied_connect_grant(
                "feishu",
                &gate_b.relied_grant_token,
            )
            .unwrap();
            assert_eq!(
                committed,
                crate::features::marketplace::ReliedGrantCommit::ReplacementPending,
                "the commit must report the rollback-capable replacement, not a secured consent"
            );
            let raw_mid = store_json();
            assert_eq!(
                raw_mid["connect_grant_tokens"]["feishu"],
                serde_json::json!(grant_c.grant_token),
                "the declined commit must leave C's pending compensation armed: {raw_mid}"
            );

            // C's own failure compensation still works in full.
            revoke_connect_enable(&grant_c).unwrap();
            let raw = store_json();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_none_or(|ids| ids.iter().all(|id| id != "feishu"))
                    && !raw["install_default_synced"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu")))
                    && raw["connect_grant_tokens"]["feishu"].is_null(),
                "the owning revoke strips C's grant, ledger, and token: {raw}"
            );
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .iter()
                    .any(|id| id == "feishu"),
                "C's failed connect ends fail-closed"
            );
        });
    }

    /// Round-8 review P2, the reverse lock order at the unit level: A's
    /// compensating revoke wins the commit race. B captures A's standing
    /// token, and A's late materialization failure strips the grant + ledger
    /// BEFORE B's commit runs — the commit must then report the loss
    /// (`GrantLost`) and write nothing, leaving the post-revoke fail-closed
    /// shape untouched.
    #[test]
    fn revoked_relied_grant_reports_lost_and_commits_nothing() {
        with_temp_home("pinvou3-skillgate-commit-lost", || {
            let grant_a = deny_first_register_connector_refresh("feishu", true).unwrap();
            assert!(grant_a.granted, "fixture: A's refresh grant lands");
            let gate_b = deny_first_register_connector("feishu", true).unwrap();
            assert_eq!(
                gate_b.relied_grant_token, grant_a.grant_token,
                "fixture: B relies on A's grant"
            );

            // A's materialization failure lands first: its compensating
            // revoke strips the grant, ledger, and ownership token B relied
            // on, before B ever commits.
            revoke_connect_enable(&grant_a).unwrap();

            let committed = crate::features::marketplace::commit_relied_connect_grant(
                "feishu",
                &gate_b.relied_grant_token,
            )
            .unwrap();
            assert_eq!(
                committed,
                crate::features::marketplace::ReliedGrantCommit::GrantLost,
                "the commit must report the vanished reliance, not a standing grant"
            );
            // The lost report must not write: the post-revoke fail-closed
            // shape stands untouched (the uninitialized scopes deny on the
            // fly now that the grant is gone).
            let raw = store_json();
            assert!(
                raw["connect_granted"]
                    .as_array()
                    .is_some_and(|ids| ids.iter().all(|id| id != "feishu"))
                    && raw["connect_grant_tokens"]["feishu"].is_null()
                    && !raw["install_default_synced"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .any(|entry| entry.as_str().is_some_and(|s| s.ends_with(":feishu"))),
                "the lost commit must leave the compensated store alone: {raw}"
            );
            assert!(
                load_disabled_bundles_for(ConnectorScope::Plain)
                    .iter()
                    .any(|id| id == "feishu"),
                "the connector must end computed-OFF (the DenyAll expansion denies it)"
            );
        });
    }

    /// Round-3 review MINOR: the lift records WHERE it found a registration
    /// marker, and the revoke restores a marker only there — the restoration
    /// is attribution-faithful on both cohorts. (a) An unmarked verdict row
    /// must come back unmarked; (b) a markered deny-first row keeps its
    /// marker.
    #[test]
    fn revoke_restores_markers_only_where_the_lift_found_one() {
        use crate::features::marketplace::scope::load_disabled_bundles_file;

        // (a) The unmarked verdict cohort.
        with_temp_home("pinvou3-skillgate-revoke-marker-unmarked", || {
            save_disabled_bundles_for(ConnectorScope::Plain, &[]).unwrap();
            save_disabled_bundles_for(ConnectorScope::Plain, &["feishu".to_string()]).unwrap();
            let file = load_disabled_bundles_file();
            assert!(
                !file
                    .default_off_scopes
                    .get("plain")
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "fixture: an entering row carries no marker: {file:?}"
            );
            let grant = deny_first_register_connector("feishu", true).unwrap();
            assert!(
                grant.lifted_markers.is_empty(),
                "fixture: the lift found no marker: {grant:?}"
            );

            revoke_connect_enable(&grant).unwrap();
            let file = load_disabled_bundles_file();
            assert!(
                file.scopes
                    .get("plain")
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the verdict row must be restored: {file:?}"
            );
            assert!(
                !file
                    .default_off_scopes
                    .get("plain")
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the restored verdict must stay unmarked — a marker would downgrade it \
                 to a liftable install default: {file:?}"
            );
        });

        // (b) The markered deny-first cohort keeps its marker.
        with_temp_home("pinvou3-skillgate-revoke-marker-markered", || {
            let store = crate::platform::paths::pinvou3_home().join("disabled_bundles.json");
            std::fs::write(
                &store,
                r#"{"scopes":{"plain":["feishu"]},"default_off_scopes":{"plain":["feishu"]},"initialized":["plain"]}"#,
            )
            .unwrap();
            let grant = deny_first_register_connector("feishu", true).unwrap();
            assert_eq!(grant.lifted_markers, vec!["plain".to_string()]);

            revoke_connect_enable(&grant).unwrap();
            let file = load_disabled_bundles_file();
            assert!(
                file.default_off_scopes
                    .get("plain")
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the markered registration restores with its marker: {file:?}"
            );
        });
    }

    /// The composer toggle-off naming the id strips the grant and writes an
    /// UNMARKED row while the grant's ledger pairs survive — the exact shape
    /// that makes the verdict reconnect-proof. The revoke's grant-consumed
    /// predicate must see the gone grant and leave the newer verdict alone:
    /// no ledger strip (which would let a reconnect lift the verdict), no
    /// marker re-add (which would downgrade the explicit verdict).
    #[test]
    fn revoke_leaves_a_consumed_grant_alone() {
        use crate::features::marketplace::scope::load_disabled_bundles_file;

        with_temp_home("pinvou3-skillgate-revoke-consumed", || {
            // plain initialized holding a MARKED feishu row (the deny-first
            // shape): the fresh-home save seeds the marker from the expansion.
            save_disabled_bundles_for(ConnectorScope::Plain, &["feishu".to_string()]).unwrap();
            let grant = deny_first_register_connector("feishu", true).unwrap();
            assert!(grant.granted, "fixture: the gesture lifts the stale row");
            assert_eq!(grant.lifted_scopes, vec!["plain".to_string()]);
            assert_eq!(grant.lifted_markers, vec!["plain".to_string()]);

            // Mid-connect: the user toggles feishu OFF again. The whole-list
            // write strips the grant, writes an unmarked row (the existing
            // markers were lifted, so nothing is retained), and keeps the
            // grant-era ledger pairs.
            save_disabled_bundles_for(ConnectorScope::Plain, &["feishu".to_string()]).unwrap();
            {
                let file = load_disabled_bundles_file();
                assert!(
                    !file
                        .default_off_scopes
                        .get("plain")
                        .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                    "fixture: the toggle-off is an explicit verdict, not a default: {file:?}"
                );
                assert!(
                    file.install_default_synced
                        .iter()
                        .any(|entry| entry == "plain:feishu"),
                    "fixture: the toggle-off keeps the ledger pair: {file:?}"
                );
                assert!(
                    !file.connect_granted.iter().any(|id| id == "feishu"),
                    "fixture: the toggle-off stripped the grant: {file:?}"
                );
            }

            revoke_connect_enable(&grant).unwrap();
            let file = load_disabled_bundles_file();
            assert!(
                file.scopes
                    .get("plain")
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the explicit verdict row must survive the compensation: {file:?}"
            );
            assert!(
                !file
                    .default_off_scopes
                    .get("plain")
                    .is_some_and(|ids| ids.iter().any(|id| id == "feishu")),
                "the compensation must not re-add a marker over the explicit verdict: {file:?}"
            );
            assert!(
                file.install_default_synced
                    .iter()
                    .any(|entry| entry == "plain:feishu"),
                "the compensation must not strip the ledger pair that makes the verdict \
                 reconnect-proof: {file:?}"
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
