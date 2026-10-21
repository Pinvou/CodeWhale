use super::*;

fn reconcile(models: &mut [SavedModel], routes: &[Route]) -> Vec<String> {
    super::reconcile(models, routes, None)
}
fn sync() -> Result<Vec<String>, String> {
    super::sync(None)
}

fn saved(id: &str, model: &str, base: &str) -> SavedModel {
    serde_json::from_value(serde_json::json!({
        "id":id, "name":"My local model", "alias":"custom label",
        "preset":"openai_compatible", "model":model, "base_url":base,
        "provider_kind":"custom", "context_window_tokens":131072,
        "max_output_tokens":4096, "reasoning_effort":"high", "vision_model_id":"vision"
    }))
    .unwrap()
}

fn route() -> Route {
    Route {
        card_id: "qwen-card".into(),
        base_url: "http://127.0.0.1:8001/v1".into(),
        model: "new-wire-id".into(),
        ready: true,
        context_repair_stamp: None,
        previous: vec![("http://127.0.0.1:8000/v1".into(), "old-wire-id".into())],
    }
}

#[test]
fn upgrade_migrates_exact_old_route_and_preserves_credentials_and_options() {
    let old = saved(
        "same-id",
        "old-wire-id",
        "http://127.0.0.1:8000/v1/chat/completions",
    );
    let mut rows = vec![old.clone()];
    assert_eq!(reconcile(&mut rows, &[route()]), ["same-id"]);
    let mut expected = old;
    expected.model = "new-wire-id".into();
    expected.base_url = "http://127.0.0.1:8001/v1".into();
    expected.model_store_card_id = Some("qwen-card".into());
    assert_eq!(rows, [expected]);
    assert!(reconcile(&mut rows, &[route()]).is_empty());
}

#[test]
fn stopped_old_route_is_bound_before_enable_overwrites_wire_identity() {
    let mut rows = vec![saved("same-id", "old-wire-id", "http://127.0.0.1:8000/v1")];
    let mut stopped = route();
    stopped.ready = false;
    reconcile(&mut rows, &[stopped]);
    assert_eq!(rows[0].model, "old-wire-id");
    assert_eq!(rows[0].model_store_card_id.as_deref(), Some("qwen-card"));
    let mut enabled = route();
    enabled.previous.clear(); // Old snapshot no longer necessary once bound.
    reconcile(&mut rows, &[enabled]);
    assert_eq!(rows[0].model, "new-wire-id");
    let mut rollback = route();
    rollback.base_url = "http://127.0.0.1:8000/v1".into();
    rollback.model = "old-wire-id".into();
    rollback.previous.clear();
    reconcile(&mut rows, &[rollback]);
    assert_eq!(rows[0].model, "old-wire-id");
    assert_eq!(rows[0].id, "same-id");
}

#[test]
fn manually_detached_historical_routes_stay_untouched_after_reload() {
    let mut detached = saved("manual", "old-wire-id", "http://127.0.0.1:8000/v1");
    detached.model_store_link_detached = true;
    let serialized = serde_json::to_string(&detached).unwrap();
    let reloaded: SavedModel = serde_json::from_str(&serialized).unwrap();
    let mut rows = vec![reloaded.clone()];
    assert!(reconcile(&mut rows, &[route()]).is_empty());
    assert_eq!(rows, [reloaded]);
    assert!(rows[0].model_store_card_id.is_none());
    // Explicitly choosing a card again opts in and restores normal migration.
    rows[0].model_store_link_detached = false;
    assert_eq!(reconcile(&mut rows, &[route()]), ["manual"]);
    assert_eq!(rows[0].model, "new-wire-id");
}

#[test]
fn remote_other_port_unrelated_and_ambiguous_legacy_routes_are_not_rewritten() {
    let rows = vec![
        saved("remote", "old-wire-id", "https://example.com/v1"),
        saved("other-local", "old-wire-id", "http://127.0.0.1:9999/v1"),
        saved("unrelated", "other-wire-id", "http://127.0.0.1:8000/v1"),
    ];
    let mut actual = rows.clone();
    assert!(reconcile(&mut actual, &[route()]).is_empty());
    assert_eq!(actual, rows);
    let mut ambiguous = vec![saved("legacy", "old-wire-id", "http://127.0.0.1:8000/v1")];
    let mut other = route();
    other.card_id = "other-card".into();
    assert!(reconcile(&mut ambiguous, &[route(), other]).is_empty());
    assert!(ambiguous[0].model_store_card_id.is_none());
}

#[test]
fn unlinked_models_are_not_created_and_bound_remote_rows_stay_unchanged() {
    assert!(reconcile(&mut [], &[route()]).is_empty());
    let mut rows = vec![saved("m", "old-wire-id", "https://example.com/v1")];
    rows[0].model_store_card_id = Some("qwen-card".into());
    let old = rows.clone();
    assert!(reconcile(&mut rows, &[route()]).is_empty());
    assert_eq!(rows, old);
}

#[test]
fn a_card_enable_does_not_rewrite_other_cards_rows() {
    let mut rows = vec![saved(
        "other-row",
        "old-wire-id",
        "http://127.0.0.1:8000/v1",
    )];
    let before = rows.clone();
    assert!(super::reconcile(&mut rows, &[route()], Some("another-card")).is_empty());
    assert_eq!(rows, before);
}

#[test]
fn endpoints_are_local_chat_bases_without_embedded_credentials_or_query() {
    for url in [
        "https://example.com/v1",
        "http://127.0.0.1/generate",
        "http://name:secret@127.0.0.1/v1",
        "http://127.0.0.1/v1?key=secret",
        "http://0.0.0.0/v1",
    ] {
        assert!(chat_base(url).is_none(), "{url}");
    }
    assert_eq!(
        chat_base("http://127.0.0.2:8000/v1/chat/completions/").as_deref(),
        Some("http://127.0.0.2:8000/v1")
    );
}

#[test]
fn optional_provenance_round_trips_without_breaking_legacy_settings() {
    let mut row = saved("m", "old-wire-id", "http://127.0.0.1:8000/v1");
    assert!(row.model_store_card_id.is_none());
    row.model_store_card_id = Some("qwen-card".into());
    assert_eq!(
        serde_json::from_str::<SavedModel>(&serde_json::to_string(&row).unwrap()).unwrap(),
        row
    );
}

fn running_card() {
    db::init_db().unwrap();
    db::add_store_model(db::AddStoreModelParams {
        card_id: "qwen-card",
        model_name: "wire",
        card_name: "Qwen",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: None,
        size_bytes: 11,
        quantization: None,
        source_repo_id: None,
        source_filename: None,
        scripts: &Default::default(),
        status: "deployed",
        profiles: &[],
        deploy_topology: super::super::store_client::DeployTopology::Single,
        card_source: "store",
    })
    .unwrap();
    db::set_endpoint_info("qwen-card", Some("http://127.0.0.1:8000/v1"), Some("wire")).unwrap();
    db::set_running("qwen-card", true).unwrap();
}

#[test]
fn explicit_follow_changes_only_target_context_and_survives_reload() {
    let _home = super::super::config::TempHome::new();
    running_card();
    let mut target = saved("target", "wire", "http://127.0.0.1:8000/v1");
    target.model_store_card_id = Some("qwen-card".into());
    target.context_window_tokens = Some(32768);
    let mut other = target.clone();
    other.id = "manual-limit".into();
    UserPrefs::update_transaction(|prefs| {
        prefs.advanced.saved_models = vec![target.clone(), other.clone()];
        prefs.advanced.active_model_id = Some(other.id.clone());
        Ok(())
    })
    .unwrap();
    assert_eq!(current_linked_ids("qwen-card"), ["target", "manual-limit"]);
    follow_deployment_context("qwen-card", "target").unwrap();
    target.context_window_tokens = None;
    let reloaded = UserPrefs::load();
    assert_eq!(reloaded.model_by_id("target"), Some(&target));
    assert_eq!(reloaded.model_by_id("manual-limit"), Some(&other));
    assert_eq!(
        reloaded.advanced.active_model_id.as_deref(),
        Some("manual-limit")
    );
    // Both a larger upgrade and a smaller rollback use the newly probed window.
    for window in [262144, 8192] {
        assert_eq!(
            crate::core::model_context::resolve_context_window(
                reloaded
                    .model_by_id("target")
                    .unwrap()
                    .context_window_tokens,
                Some(window),
                None,
            )
            .0,
            Some(window)
        );
    }
    assert_eq!(
        crate::core::model_context::resolve_context_window(
            other.context_window_tokens,
            Some(262144),
            None,
        )
        .0,
        Some(32768)
    );
    assert!(sync().unwrap().is_empty()); // Never write a probe back as a fixed cap.
    assert!(
        UserPrefs::load()
            .model_by_id("target")
            .unwrap()
            .context_window_tokens
            .is_none()
    );
}

#[test]
fn follow_rejects_stale_unlinked_detached_and_stopped_models_without_mutation() {
    let _home = super::super::config::TempHome::new();
    running_card();
    let mut row = saved("target", "wire", "http://127.0.0.1:8000/v1");
    row.model_store_card_id = Some("qwen-card".into());
    let original = row.clone();
    for mode in [
        "stale",
        "remote",
        "unlinked",
        "detached",
        "stopped",
        "wrong-card",
    ] {
        let mut row = original.clone();
        match mode {
            "stale" => row.model = "old-wire".into(),
            "remote" => row.base_url = "https://example.com/v1".into(),
            "unlinked" => row.model_store_card_id = None,
            "detached" => row.model_store_link_detached = true,
            "stopped" => db::set_running("qwen-card", false).unwrap(),
            _ => row.model_store_card_id = Some("another-card".into()),
        }
        UserPrefs::update_transaction(|prefs| {
            prefs.advanced.saved_models = vec![row.clone()];
            Ok(())
        })
        .unwrap();
        let before = UserPrefs::load();
        assert!(
            follow_deployment_context("qwen-card", "target").is_err(),
            "{mode}"
        );
        assert!(current_linked_ids("qwen-card").is_empty(), "{mode}");
        assert_eq!(
            UserPrefs::load().advanced.saved_models,
            before.advanced.saved_models
        );
        db::set_running("qwen-card", true).unwrap();
    }
}

#[test]
fn sync_commits_existing_rows_and_selection_and_reloads_after_restart() {
    let _home = super::super::config::TempHome::new();
    db::init_db().unwrap();
    let scripts = Default::default();
    db::add_store_model(db::AddStoreModelParams {
        card_id: "qwen-card",
        model_name: "card-model",
        card_name: "Qwen",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: None,
        size_bytes: 11,
        quantization: None,
        source_repo_id: None,
        source_filename: None,
        scripts: &scripts,
        status: "deployed",
        profiles: &[],
        deploy_topology: super::super::store_client::DeployTopology::Single,
        card_source: "store",
    })
    .unwrap();
    db::set_endpoint_info(
        "qwen-card",
        Some("http://127.0.0.1:8000/v1"),
        Some("old-wire-id"),
    )
    .unwrap();
    UserPrefs::update_transaction(|prefs| {
        prefs.advanced.saved_models = vec![
            saved("keep-selected", "other", "https://example.com/v1"),
            saved("stable-id", "old-wire-id", "http://127.0.0.1:8000/v1"),
        ];
        prefs.advanced.active_model_id = Some("keep-selected".into());
        Ok(())
    })
    .unwrap();
    assert_eq!(sync().unwrap(), ["stable-id"]); // stopped legacy model gets durable provenance
    db::set_endpoint_info(
        "qwen-card",
        Some("http://127.0.0.1:8001/v1/chat/completions"),
        Some("new-wire-id"),
    )
    .unwrap();
    db::set_running("qwen-card", true).unwrap();
    assert!(matches_current_model_route(
        "qwen-card",
        "http://127.0.0.1:8001/v1/chat/completions",
        "new-wire-id"
    ));
    assert!(!matches_current_model_route(
        "qwen-card",
        "http://127.0.0.1:9999/v1",
        "new-wire-id"
    ));
    assert!(!matches_current_model_route(
        "qwen-card",
        "http://127.0.0.1:8001/v1",
        "other-wire-id"
    ));
    assert!(!matches_current_model_route(
        "missing-card",
        "http://127.0.0.1:8001/v1",
        "new-wire-id"
    ));
    assert_eq!(sync().unwrap(), ["stable-id"]);
    let reloaded = UserPrefs::load();
    assert_eq!(
        reloaded.advanced.active_model_id.as_deref(),
        Some("keep-selected")
    );
    let row = reloaded.model_by_id("stable-id").unwrap();
    assert_eq!(row.model, "new-wire-id");
    assert_eq!(row.base_url, "http://127.0.0.1:8001/v1");
    assert_eq!(row.context_window_tokens, Some(131072));
    assert_eq!(row.max_output_tokens, Some(4096));
    assert_eq!(row.model_store_card_id.as_deref(), Some("qwen-card"));
    assert!(sync().unwrap().is_empty());
    assert_eq!(
        UserPrefs::load().advanced.active_model_id,
        reloaded.advanced.active_model_id
    );
}

#[test]
fn archived_version_identity_repairs_an_already_upgraded_legacy_row() {
    let _home = super::super::config::TempHome::new();
    db::init_db().unwrap();
    db::add_store_model(db::AddStoreModelParams {
        card_id: "qwen-card",
        model_name: "card-model",
        card_name: "Qwen",
        org: None,
        description: None,
        engine_name: "native",
        engine_version: None,
        size_bytes: 11,
        quantization: None,
        source_repo_id: None,
        source_filename: None,
        scripts: &Default::default(),
        status: "deployed",
        profiles: &[],
        deploy_topology: super::super::store_client::DeployTopology::Single,
        card_source: "store",
    })
    .unwrap();
    db::set_endpoint_info(
        "qwen-card",
        Some("http://127.0.0.1:8000/v1"),
        Some("old-wire-id"),
    )
    .unwrap();
    let old = db::get_store_model("qwen-card").unwrap().unwrap();
    version_store::put(&version_store::Version {
        planned_bytes: 0,
        card_id: "qwen-card".into(),
        version_id: "old-version".into(),
        state: "history".into(),
        record: Some(old),
        models_root: None,
        runtime_card_id: "old-runtime".into(),
        resource_dir: None,
        managed: false,
        created_at: "2026-10-19".into(),
        error: None,
    })
    .unwrap();
    db::set_endpoint_info(
        "qwen-card",
        Some("http://127.0.0.1:8001/v1"),
        Some("new-wire-id"),
    )
    .unwrap();
    db::set_running("qwen-card", true).unwrap();
    UserPrefs::update_transaction(|prefs| {
        prefs.advanced.saved_models = vec![saved(
            "stable-id",
            "old-wire-id",
            "http://127.0.0.1:8000/v1",
        )];
        prefs.advanced.active_model_id = Some("stable-id".into());
        Ok(())
    })
    .unwrap();
    assert_eq!(sync().unwrap(), ["stable-id"]);
    assert_eq!(
        UserPrefs::load().model_by_id("stable-id").unwrap().model,
        "new-wire-id"
    );
    assert_eq!(
        UserPrefs::load().advanced.active_model_id.as_deref(),
        Some("stable-id")
    );
}

#[test]
fn store_routes_only_accept_http_chat_endpoints() {
    assert!(chat_base("http://127.0.0.1:8000/v1").is_some());
    assert!(chat_base("https://localhost:8000/v1/chat/completions").is_some());
    assert!(chat_base("ftp://127.0.0.1:8000/v1").is_none());
    assert!(chat_base("ws://127.0.0.1:8000/v1").is_none());
}
