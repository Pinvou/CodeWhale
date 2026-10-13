use super::{
    CODE_EXECUTION_DESCRIPTION, DEFAULT_ACTIVE_NATIVE_TOOLS, DeferredHydration,
    allowlist_is_native_file_and_shell_only, apply_mcp_tool_deferral, apply_native_tool_deferral,
    apply_registry_first_shell_guidance, build_model_tool_catalog_with_surface,
    default_synthetic_catalog_tool_names, ensure_advanced_tooling, execute_tool_search_with_cache,
    initial_active_tools, is_synthetic_catalog_tool, maybe_hydrate_requested_deferred_tool,
    remove_toolbox_drops, render_toolbox_eviction_notes, tool_matches_any_rule,
    toolbox_oversize_reason, toolbox_oversize_search_hint, touch_cached_tool_after_execution,
};
use crate::core::session::ToolActivationCache;
use crate::models::Tool;
use crate::tui::app::AppMode;
use serde_json::json;
use std::collections::{BTreeSet, HashSet};

fn tool(name: &str) -> Tool {
    Tool {
        tool_type: None,
        name: name.to_string(),
        description: format!("{name} test tool"),
        input_schema: json!({"type": "object", "properties": {}}),
        allowed_callers: None,
        defer_loading: None,
        input_examples: None,
        strict: None,
        cache_control: None,
    }
}

/// `code_execution` writes the script to a tempdir and runs it as a plain
/// child process in the workspace — no seccomp, no jail, no container. The
/// description is model-facing, so calling it a sandbox would tell the model
/// it has isolation the runtime never provides.
#[test]
fn code_execution_description_does_not_claim_process_sandboxing() {
    assert!(CODE_EXECUTION_DESCRIPTION.contains("local Python interpreter"));
    assert!(!CODE_EXECUTION_DESCRIPTION.contains("sandbox"));
}

/// The published synthetic-name list and the predicate that classifies a
/// catalog entry as synthetic must agree. A name that appears in the list
/// but is not classified synthetic would let the request projection report
/// a provenance the engine itself disputes.
#[test]
fn published_synthetic_names_agree_with_the_synthetic_predicate() {
    let names = default_synthetic_catalog_tool_names();
    assert!(!names.is_empty());
    for name in &names {
        assert!(
            is_synthetic_catalog_tool(name),
            "'{name}' is published as synthetic but the predicate disagrees"
        );
    }
    let mut sorted = names.clone();
    sorted.sort();
    sorted.dedup();
    assert_eq!(names, sorted, "the list must be sorted and deduplicated");

    // MCP names resolve through the real pool, so they are deliberately
    // absent here even though the predicate accepts them.
    assert!(!names.iter().any(|name| name.starts_with("mcp_")));

    // `multi_tool_use.parallel` is a call name, never a catalog entry, so
    // it has no catalog provenance and must not be published as synthetic.
    assert!(
        !names
            .iter()
            .any(|name| name == super::MULTI_TOOL_PARALLEL_NAME)
    );
}

#[test]
fn first_turn_surface_is_stable_across_plan_work_and_full_access() {
    assert_eq!(
        DEFAULT_ACTIVE_NATIVE_TOOLS,
        &["read", "write", "edit", "bash", "agent", "todo_write"]
    );
    let expected = [
        "agent",
        "bash",
        "edit",
        "read",
        "todo_write",
        "tool_search",
        "write",
    ]
    .into_iter()
    .map(str::to_string)
    .collect::<BTreeSet<_>>();
    for mode in [AppMode::Plan, AppMode::Agent] {
        let mut catalog = [
            "read",
            "write",
            "edit",
            "bash",
            "agent",
            "todo_write",
            "Git",
            "Run",
            "tasks",
            "load_skill",
        ]
        .into_iter()
        .map(tool)
        .collect::<Vec<_>>();
        let always_load = HashSet::new();
        apply_native_tool_deferral(&mut catalog, &always_load);
        ensure_advanced_tooling(&mut catalog, mode, &always_load);
        let active = initial_active_tools(&catalog)
            .into_iter()
            .collect::<BTreeSet<_>>();
        assert_eq!(active, expected, "{mode:?}");
    }
}

#[test]
fn mcp_tools_are_searchable_not_eager_in_every_mode() {
    for mode in [AppMode::Plan, AppMode::Agent] {
        let mut catalog = vec![tool("read_mcp_resource"), tool("mcp_acme_lookup")];
        apply_mcp_tool_deferral(&mut catalog, mode, &HashSet::new());
        assert!(
            catalog
                .iter()
                .all(|definition| definition.defer_loading == Some(true)),
            "{mode:?}: {catalog:?}"
        );
    }
}

#[test]
fn cache_eviction_does_not_hide_a_tool_that_became_eager() {
    let mut catalog = vec![tool("promoted")];
    catalog[0].defer_loading = Some(true);
    let mut cache = ToolActivationCache::default();
    cache.activate(&catalog, &["promoted".to_string()]);

    catalog[0].defer_loading = Some(false);
    let mut active = initial_active_tools(&catalog);
    let removals = cache.revalidate(&catalog);
    remove_toolbox_drops(&mut active, removals.dropped_names().map(str::to_string));

    assert!(active.contains("promoted"));
    assert_eq!(cache.names().count(), 0);
    // The promotion is not an unavailability: reporting it as gone would be
    // a false statement, so neither removal bucket may name it.
    assert!(removals.stale.is_empty());
    assert!(removals.bounds_evicted.is_empty());
}

#[test]
fn successful_cached_execution_updates_lru_without_granting_uncached_names() {
    let catalog = (0..=8)
        .map(|index| {
            let mut definition = tool(&format!("deferred-{index}"));
            definition.defer_loading = Some(true);
            definition
        })
        .collect::<Vec<_>>();
    let mut cache = ToolActivationCache::default();
    let first = (0..8)
        .map(|index| format!("deferred-{index}"))
        .collect::<Vec<_>>();
    let mut active = HashSet::new();
    let delta = cache.activate(&catalog, &first);
    active.extend(delta.admitted);

    assert!(
        touch_cached_tool_after_execution(&catalog, &mut active, &mut cache, "deferred-0").changed
    );
    let delta = cache.activate(&catalog, &["deferred-8".to_string()]);
    remove_toolbox_drops(&mut active, delta.dropped_names().map(str::to_string));
    active.extend(delta.admitted);
    assert!(cache.names().any(|name| name == "deferred-0"));
    assert!(!cache.names().any(|name| name == "deferred-1"));

    assert!(
        !touch_cached_tool_after_execution(&catalog, &mut active, &mut cache, "never-activated")
            .changed
    );
    assert!(!active.contains("never-activated"));
}

#[test]
fn searching_for_an_eager_tool_is_not_reported_as_cache_rejected() {
    let mut catalog = vec![tool("read")];
    catalog[0].defer_loading = Some(false);
    let mut active = initial_active_tools(&catalog);
    let mut cache = ToolActivationCache::default();

    let result = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "read"}),
        &catalog,
        &mut active,
        &mut cache,
        None,
    )
    .expect("tool search should succeed");

    let metadata = result.metadata.expect("search metadata");
    assert_eq!(metadata["tool_references"], json!([]));
    assert_eq!(metadata["unavailable_tool_references"], json!([]));
    assert!(active.contains("read"));
    assert_eq!(cache.names().count(), 0);
}

#[test]
fn allow_and_deny_rules_cover_visible_and_hidden_compat_aliases_symmetrically() {
    for family in [
        &["read", "read_file"][..],
        &["write", "write_file"][..],
        &["edit", "edit_file"][..],
        &["bash", "Bash", "exec_shell"][..],
    ] {
        for rule in family {
            let rules = vec![(*rule).to_string()];
            for tool_name in family {
                assert!(
                    tool_matches_any_rule(&rules, tool_name),
                    "rule {rule:?} should cover alias {tool_name:?}"
                );
            }
        }
    }

    assert!(tool_matches_any_rule(&["exec_shell*".to_string()], "bash"));
    assert!(tool_matches_any_rule(
        &["exec_shell*".to_string()],
        "exec_shell_wait"
    ));
    for primitive in ["read", "write", "edit"] {
        assert!(tool_matches_any_rule(&["File".to_string()], primitive));
    }
    assert!(!tool_matches_any_rule(&["read".to_string()], "write"));
}

#[test]
fn native_file_and_shell_allowlist_can_skip_mcp_startup() {
    let native = ["bash", "read", "write", "edit"].map(str::to_string);
    assert!(allowlist_is_native_file_and_shell_only(Some(&native)));
    assert!(!allowlist_is_native_file_and_shell_only(None));
}

#[test]
fn unknown_and_wildcard_allowlists_keep_mcp_startup() {
    for rule in ["mcp_github_list_prs", "mcp_*", "m*", "*", "other_tool"] {
        assert!(
            !allowlist_is_native_file_and_shell_only(Some(&[rule.to_string()])),
            "{rule} may admit an MCP-backed tool"
        );
    }
}

#[test]
fn compact_surface_keeps_the_exact_eager_agent_head() {
    let catalog = build_model_tool_catalog_with_surface(
        ["read", "write", "edit", "bash", "agent", "todo_write"]
            .into_iter()
            .map(tool)
            .collect(),
        Vec::new(),
        AppMode::Agent,
        &HashSet::new(),
        crate::model_profile::ToolSurfaceBudget::Compact,
    );

    assert_eq!(
        catalog
            .iter()
            .find(|definition| definition.name == "agent")
            .and_then(|definition| definition.defer_loading),
        Some(false)
    );
}

#[test]
fn registry_first_guidance_does_not_expand_contract_bash_schema_text() {
    let mut catalog = vec![tool("bash")];
    let description = catalog[0].description.clone();

    apply_registry_first_shell_guidance(&mut catalog);

    assert_eq!(catalog[0].description, description);
}

/// Boot-window status (#588): while MCP servers are still connecting, a miss
/// must read as "not yet", not "absent". The result therefore carries the
/// connecting server names and the retry contract.
#[test]
fn booting_search_result_names_the_still_connecting_servers() {
    let catalog = vec![tool("read")];
    let mut active = initial_active_tools(&catalog);
    let mut cache = ToolActivationCache::default();
    let connecting = vec!["canva_mcp".to_string(), "qcc-company".to_string()];

    let result = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "make pptx"}),
        &catalog,
        &mut active,
        &mut cache,
        Some(&connecting),
    )
    .expect("tool search should succeed");

    let payload: serde_json::Value =
        serde_json::from_str(&result.content).expect("payload is JSON");
    assert_eq!(payload["mcp_boot"]["status"], "connecting");
    assert_eq!(
        payload["mcp_boot"]["servers_pending"],
        json!(["canva_mcp", "qcc-company"])
    );
    let note = payload["mcp_boot"]["note"].as_str().expect("retry note");
    assert!(
        note.contains("still connecting") && note.contains("again"),
        "the note must state the boot window and the retry contract: {note}"
    );
    let metadata = result.metadata.expect("search metadata");
    assert_eq!(
        metadata["mcp_boot"]["servers_pending"],
        payload["mcp_boot"]["servers_pending"]
    );
}

/// A settled boot must keep the exact legacy result shape: no booting block
/// when no status is passed, and none for an empty connecting list either —
/// an empty list means everything settled, so "still connecting" would be a
/// lie.
#[test]
fn settled_search_result_carries_no_booting_block() {
    let catalog = vec![tool("read")];
    for status in [None, Some(&[] as &[String])] {
        let mut active = initial_active_tools(&catalog);
        let mut cache = ToolActivationCache::default();

        let result = execute_tool_search_with_cache(
            super::TOOL_SEARCH_NAME,
            &json!({"query": "read"}),
            &catalog,
            &mut active,
            &mut cache,
            status,
        )
        .expect("tool search should succeed");

        let payload: serde_json::Value =
            serde_json::from_str(&result.content).expect("payload is JSON");
        assert!(
            payload.get("mcp_boot").is_none(),
            "a settled boot must not attach a booting block: {payload}"
        );
    }
}

/// The activation cache is bounded, so a search that admits new tools can
/// revoke ones the conversation already used. The result must name those
/// evictions — a tool that silently vanishes between turns is exactly the
/// "reference returned but never callable" trap. Searches without evictions
/// keep the legacy result shape with no eviction block at all.
#[test]
fn search_result_names_cache_evictions_instead_of_dropping_them_silently() {
    let catalog: Vec<Tool> = (0..10)
        .map(|index| {
            let mut definition = tool(&format!("deferred-{index}"));
            definition.defer_loading = Some(true);
            definition
        })
        .collect();
    let mut active = initial_active_tools(&catalog);
    let mut cache = ToolActivationCache::default();

    let first = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "deferred", "max_results": 8}),
        &catalog,
        &mut active,
        &mut cache,
        None,
    )
    .expect("first search should succeed");
    let first_payload: serde_json::Value =
        serde_json::from_str(&first.content).expect("payload is JSON");
    assert_eq!(
        first_payload["tool_references"].as_array().unwrap().len(),
        8
    );
    assert!(
        first_payload.get("evicted_tool_references").is_none(),
        "a search without evictions must keep the legacy shape: {first_payload}"
    );
    assert!(active.contains("deferred-0"));
    assert_eq!(cache.names().count(), 8);

    // Naming deferred-8/9 admits two new tools into the full cache and must
    // evict the two least-recently-used activations.
    let second = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "deferred-8 deferred-9", "max_results": 8}),
        &catalog,
        &mut active,
        &mut cache,
        None,
    )
    .expect("second search should succeed");
    let second_payload: serde_json::Value =
        serde_json::from_str(&second.content).expect("payload is JSON");
    assert_eq!(
        second_payload["evicted_tool_references"],
        json!([
            {"type": "tool_reference", "tool_name": "deferred-0"},
            {"type": "tool_reference", "tool_name": "deferred-1"},
        ])
    );
    let note = second_payload["toolbox_note"].as_str().expect("note");
    assert!(
        note.contains("deferred-0") && note.contains("no longer callable"),
        "the note must name the evictions and the recovery contract: {note}"
    );
    assert!(!active.contains("deferred-0"));
    assert!(!active.contains("deferred-1"));
    assert!(active.contains("deferred-8"));
    let metadata = second.metadata.expect("search metadata");
    assert_eq!(
        metadata["evicted_tool_references"],
        json!(["deferred-0", "deferred-1"])
    );
    assert_eq!(metadata["toolbox_note"], second_payload["toolbox_note"]);
}

/// The serialized-bytes bound evicts too, not just the eight-name bound, and
/// the reporting must not care which bound fired.
#[test]
fn search_result_names_byte_bound_cache_evictions_too() {
    let heavy = |name: &str, description_bytes: usize| {
        let mut definition = tool(name);
        definition.defer_loading = Some(true);
        definition.description = "x".repeat(description_bytes);
        definition
    };
    let catalog = vec![
        heavy("deferred-small", 10_000),
        heavy("deferred-large", 8_000),
    ];
    let mut active = initial_active_tools(&catalog);
    let mut cache = ToolActivationCache::default();

    execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^deferred-small", "match": "regex"}),
        &catalog,
        &mut active,
        &mut cache,
        None,
    )
    .expect("first search should succeed");
    assert_eq!(cache.names().collect::<Vec<_>>(), vec!["deferred-small"]);

    let second = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^deferred-large", "match": "regex"}),
        &catalog,
        &mut active,
        &mut cache,
        None,
    )
    .expect("second search should succeed");
    let payload: serde_json::Value =
        serde_json::from_str(&second.content).expect("payload is JSON");
    // Both tools sit under the per-tool cap, but together they overflow the
    // 16KiB cache budget, so admitting the second evicts the first.
    assert_eq!(
        payload["evicted_tool_references"],
        json!([{"type": "tool_reference", "tool_name": "deferred-small"}])
    );
    let note = payload["toolbox_note"].as_str().expect("note");
    assert!(
        note.contains("deferred-small") && note.contains("16KiB"),
        "the note must name the byte-bound eviction: {note}"
    );
    assert!(!active.contains("deferred-small"));
    let metadata = second.metadata.expect("search metadata");
    assert_eq!(
        metadata["evicted_tool_references"],
        json!(["deferred-small"])
    );
}

/// A search after the catalog shrank must report the dropped activation under
/// the unavailable vocabulary — the eviction note's "re-search to
/// re-activate" contract would be a false promise for a tool that is gone
/// from the catalog.
#[test]
fn search_reports_catalog_stale_evictions_as_unavailable_not_evicted() {
    let catalog_v1: Vec<Tool> = (0..3)
        .map(|index| {
            let mut definition = tool(&format!("deferred-{index}"));
            definition.defer_loading = Some(true);
            definition
        })
        .collect();
    let mut active = initial_active_tools(&catalog_v1);
    let mut cache = ToolActivationCache::default();
    execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "deferred", "max_results": 8}),
        &catalog_v1,
        &mut active,
        &mut cache,
        None,
    )
    .expect("first search should succeed");
    assert!(active.contains("deferred-0"));

    let catalog_v2: Vec<Tool> = catalog_v1
        .into_iter()
        .filter(|definition| definition.name != "deferred-0")
        .collect();
    let second = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^deferred-1", "match": "regex"}),
        &catalog_v2,
        &mut active,
        &mut cache,
        None,
    )
    .expect("second search should succeed");
    let payload: serde_json::Value =
        serde_json::from_str(&second.content).expect("payload is JSON");
    let unavailable = payload["unavailable_tool_references"]
        .as_array()
        .expect("unavailable references");
    let stale_entry = unavailable
        .iter()
        .find(|reference| reference["tool_name"] == "deferred-0")
        .expect("the stale eviction must be reported");
    assert_eq!(
        stale_entry["reason"],
        super::TOOLBOX_CATALOG_SHRINK_REASON,
        "the stale reason must be the catalog-shrink one, not the bounds one: {stale_entry}"
    );
    assert!(
        payload.get("evicted_tool_references").is_none() && payload.get("toolbox_note").is_none(),
        "a staleness eviction is not a bounds eviction: {payload}"
    );
    assert!(!active.contains("deferred-0"));
    let metadata = second.metadata.expect("search metadata");
    assert!(
        metadata["unavailable_tool_references"]
            .as_array()
            .expect("metadata unavailable references")
            .iter()
            .any(|reference| reference["tool_name"] == "deferred-0")
    );
}

/// A cached tool promoted to eager stays directly callable: the search that
/// forgets the cache entry must not report it as unavailable or evicted —
/// the same request already carries its schema in the tools head, so both
/// vocabularies would be false statements.
#[test]
fn search_does_not_report_an_eager_promotion_as_unavailable() {
    let mut catalog = vec![tool("promoted")];
    catalog[0].defer_loading = Some(true);
    let mut active = initial_active_tools(&catalog);
    let mut cache = ToolActivationCache::default();
    execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^promoted", "match": "regex"}),
        &catalog,
        &mut active,
        &mut cache,
        None,
    )
    .expect("first search should succeed");
    assert!(active.contains("promoted"));

    catalog[0].defer_loading = Some(false);
    let second = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^promoted", "match": "regex"}),
        &catalog,
        &mut active,
        &mut cache,
        None,
    )
    .expect("second search should succeed");
    let payload: serde_json::Value =
        serde_json::from_str(&second.content).expect("payload is JSON");
    let unavailable = payload["unavailable_tool_references"]
        .as_array()
        .expect("unavailable references");
    assert!(
        !unavailable
            .iter()
            .any(|reference| reference["tool_name"] == "promoted"),
        "an eager promotion is not an unavailability: {unavailable:?}"
    );
    assert!(
        payload.get("evicted_tool_references").is_none() && payload.get("toolbox_note").is_none(),
        "an eager promotion is not a bounds eviction: {payload}"
    );
    assert!(active.contains("promoted"));
    assert_eq!(cache.names().count(), 0);
}

/// A cached tool whose schema grows past the collective budget (an MCP
/// refresh shipping a larger schema) is still a cataloged, re-activatable
/// deferred member: the next search must pop it under the eviction note, not
/// as an unavailability.
#[test]
fn search_reports_growth_overflows_as_re_activatable_evictions() {
    let heavy = |name: &str, description_bytes: usize| {
        let mut definition = tool(name);
        definition.defer_loading = Some(true);
        definition.description = "x".repeat(description_bytes);
        definition
    };
    let catalog_v1 = vec![heavy("deferred-left", 8), heavy("deferred-right", 6_000)];
    let mut active = initial_active_tools(&catalog_v1);
    let mut cache = ToolActivationCache::default();
    execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "deferred", "max_results": 8}),
        &catalog_v1,
        &mut active,
        &mut cache,
        None,
    )
    .expect("first search should succeed");
    assert_eq!(
        cache.names().collect::<Vec<_>>(),
        vec!["deferred-left", "deferred-right"]
    );

    let catalog_v2 = vec![
        heavy("deferred-left", 12_000),
        heavy("deferred-right", 6_000),
    ];
    let second = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^deferred-right", "match": "regex"}),
        &catalog_v2,
        &mut active,
        &mut cache,
        None,
    )
    .expect("second search should succeed");
    let payload: serde_json::Value =
        serde_json::from_str(&second.content).expect("payload is JSON");
    assert_eq!(
        payload["evicted_tool_references"],
        json!([{"type": "tool_reference", "tool_name": "deferred-left"}]),
        "the growth overflow must be reported as a re-activatable eviction: {payload}"
    );
    let unavailable = payload["unavailable_tool_references"]
        .as_array()
        .expect("unavailable references");
    assert!(
        !unavailable
            .iter()
            .any(|reference| reference["tool_name"] == "deferred-left"),
        "the growth victim is still cataloged, so the unavailable vocabulary would lie: {unavailable:?}"
    );
    assert!(!active.contains("deferred-left"));
    assert!(active.contains("deferred-right"));
}

/// A cached tool whose schema grows past the per-tool budget is unavailable
/// rather than evicted, and the report must say exactly that — not claim the
/// tool left the catalog. The discovery-time rejection and the revalidation
/// drop describe the same condition, so the payload carries one entry.
#[test]
fn search_reports_oversize_drops_with_the_oversize_reason() {
    let heavy = |name: &str, description_bytes: usize| {
        let mut definition = tool(name);
        definition.defer_loading = Some(true);
        definition.description = "x".repeat(description_bytes);
        definition
    };
    let catalog_v1 = vec![heavy("deferred-left", 8)];
    let mut active = initial_active_tools(&catalog_v1);
    let mut cache = ToolActivationCache::default();
    execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^deferred-left", "match": "regex"}),
        &catalog_v1,
        &mut active,
        &mut cache,
        None,
    )
    .expect("first search should succeed");
    assert_eq!(cache.names().collect::<Vec<_>>(), vec!["deferred-left"]);

    let catalog_v2 = vec![heavy("deferred-left", 20_000)];
    let second = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^deferred-left", "match": "regex"}),
        &catalog_v2,
        &mut active,
        &mut cache,
        None,
    )
    .expect("second search should succeed");
    let payload: serde_json::Value =
        serde_json::from_str(&second.content).expect("payload is JSON");
    let unavailable = payload["unavailable_tool_references"]
        .as_array()
        .expect("unavailable references");
    let entries: Vec<&serde_json::Value> = unavailable
        .iter()
        .filter(|reference| reference["tool_name"] == "deferred-left")
        .collect();
    assert_eq!(
        entries.len(),
        1,
        "the oversize drop must be reported exactly once: {unavailable:?}"
    );
    assert_eq!(
        entries[0]["reason"],
        format!(
            "{}{}",
            toolbox_oversize_reason(),
            toolbox_oversize_search_hint()
        ),
        "the oversize drop must carry the oversize reason with its search-context recovery hint, not the catalog-shrink one: {entries:?}"
    );
    assert!(
        payload.get("evicted_tool_references").is_none() && payload.get("toolbox_note").is_none(),
        "an oversize drop is not a re-activatable eviction: {payload}"
    );
    assert!(!active.contains("deferred-left"));
}

/// The searched tool itself can be the LRU-front victim of a growth
/// overflow: revalidation pops it, the admission loop re-admits it, and the
/// same payload must not name it evicted while also handing it back in
/// `tool_references`.
#[test]
fn search_does_not_name_its_own_admission_as_evicted() {
    let heavy = |name: &str, description_bytes: usize| {
        let mut definition = tool(name);
        definition.defer_loading = Some(true);
        definition.description = "x".repeat(description_bytes);
        definition
    };
    let catalog_v1 = vec![heavy("deferred-left", 8), heavy("deferred-right", 6_000)];
    let mut active = initial_active_tools(&catalog_v1);
    let mut cache = ToolActivationCache::default();
    execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "deferred", "max_results": 8}),
        &catalog_v1,
        &mut active,
        &mut cache,
        None,
    )
    .expect("first search should succeed");
    assert_eq!(
        cache.names().collect::<Vec<_>>(),
        vec!["deferred-left", "deferred-right"]
    );

    let catalog_v2 = vec![
        heavy("deferred-left", 12_000),
        heavy("deferred-right", 6_000),
    ];
    let second = execute_tool_search_with_cache(
        super::TOOL_SEARCH_NAME,
        &json!({"query": "^deferred-left", "match": "regex"}),
        &catalog_v2,
        &mut active,
        &mut cache,
        None,
    )
    .expect("second search should succeed");
    let payload: serde_json::Value =
        serde_json::from_str(&second.content).expect("payload is JSON");
    let references = payload["tool_references"].as_array().expect("references");
    assert!(
        references
            .iter()
            .any(|reference| reference["tool_name"] == "deferred-left"),
        "the searched growth victim must be re-admitted by this very result: {references:?}"
    );
    assert_eq!(
        payload["evicted_tool_references"],
        json!([{"type": "tool_reference", "tool_name": "deferred-right"}]),
        "only the net eviction may be named: {payload}"
    );
    let note = payload["toolbox_note"].as_str().expect("note");
    assert!(
        note.contains("deferred-right") && !note.contains("deferred-left"),
        "the note must not claim the re-admitted tool is uncallable: {note}"
    );
    assert!(active.contains("deferred-left"));
    assert!(!active.contains("deferred-right"));
    let metadata = second.metadata.expect("search metadata");
    assert_eq!(
        metadata["evicted_tool_references"],
        json!(["deferred-right"])
    );
}

/// A direct call to an oversize deferred tool can never enter the bounded
/// toolbox, so the hydration receipt must refuse with the shared oversize
/// reason instead of promising a load the admission would refuse.
#[test]
fn hydration_of_an_oversize_tool_reports_the_rejection_not_a_load() {
    let mut oversize = tool("huge-deferred");
    oversize.defer_loading = Some(true);
    oversize.description = "x".repeat(20_000);
    let catalog = vec![oversize];
    // An empty active set: in a real turn core tools fill it, and the
    // single-tool fallback must not pre-activate the tool under test.
    let active = HashSet::new();
    let mut hydrated = HashSet::new();

    let Some(DeferredHydration::Refused(receipt)) = maybe_hydrate_requested_deferred_tool(
        "huge-deferred",
        &json!({}),
        &catalog,
        &active,
        &mut hydrated,
    ) else {
        panic!("the oversize call must be refused, not loaded");
    };
    assert!(
        receipt
            .content
            .contains("was not loaded and was not executed"),
        "the receipt must refuse the load: {}",
        receipt.content
    );
    assert!(
        receipt.content.contains(&toolbox_oversize_reason()),
        "the refusal must carry the shared oversize reason: {}",
        receipt.content
    );
    assert!(
        !receipt.content.contains("has now been loaded"),
        "the receipt must not promise a load: {}",
        receipt.content
    );
    assert!(
        hydrated.is_empty(),
        "an oversize tool must not be offered to the batch activation: {hydrated:?}"
    );
}

/// Ride-time netting: a parked note must not name a tool the riding result
/// hands back. Names back in the activation cache — or in the request's
/// active tool set, which a mid-turn MCP catalog refresh can repopulate
/// without re-activating the cache — are dropped from the note, a note
/// emptied by the netting is dropped entirely, and the surviving note keeps
/// its own trigger attribution.
#[test]
fn toolbox_note_rendering_nets_re_admitted_names_at_ride_time() {
    let mut deferred = tool("deferred-x");
    deferred.defer_loading = Some(true);
    let catalog = vec![deferred];
    let mut cache = ToolActivationCache::default();
    cache.activate(&catalog, &["deferred-x".to_string()]);

    let note = |trigger, names| crate::core::session::PendingToolboxEviction {
        trigger,
        evicted: names,
    };
    use crate::core::session::ToolboxEvictionTrigger;
    let mut notes = vec![
        note(
            ToolboxEvictionTrigger::TurnStart,
            vec!["deferred-x".to_string(), "deferred-y".to_string()],
        ),
        note(ToolboxEvictionTrigger::Step, vec!["deferred-x".to_string()]),
    ];
    let rendered = render_toolbox_eviction_notes(&mut notes, &cache, &HashSet::new())
        .expect("deferred-y is still evicted");
    assert!(
        rendered.contains("deferred-y") && !rendered.contains("deferred-x"),
        "re-admitted names must be netted out of the note: {rendered}"
    );
    assert!(
        rendered.contains("This turn's start evicted"),
        "the surviving note keeps its own trigger attribution: {rendered}"
    );

    // A note whose every name is re-admitted is fully absorbed: reporting
    // it would claim a callable tool is uncallable.
    let mut notes = vec![note(
        ToolboxEvictionTrigger::Step,
        vec!["deferred-x".to_string()],
    )];
    assert!(
        render_toolbox_eviction_notes(&mut notes, &cache, &HashSet::new()).is_none(),
        "a fully netted note must not ride at all"
    );

    // A name the mid-turn MCP refresh re-advertised into the active set is
    // callable even though the cache no longer holds it: the note must net
    // against the active set too, or it would contradict the next request,
    // which carries the tool.
    let mut active = HashSet::new();
    active.insert("deferred-y".to_string());
    let mut notes = vec![note(
        ToolboxEvictionTrigger::Step,
        vec!["deferred-y".to_string()],
    )];
    assert!(
        render_toolbox_eviction_notes(&mut notes, &ToolActivationCache::default(), &active)
            .is_none(),
        "an active-set name is callable and must not be declared evicted"
    );
}
