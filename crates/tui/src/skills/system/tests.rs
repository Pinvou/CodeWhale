use super::*;
use tempfile::TempDir;

fn skill_file(tmp: &TempDir, name: &str) -> std::path::PathBuf {
    tmp.path().join(name).join("SKILL.md")
}

fn skill_dir(tmp: &TempDir, name: &str) -> std::path::PathBuf {
    tmp.path().join(name)
}

fn marker_file(tmp: &TempDir) -> std::path::PathBuf {
    tmp.path().join(".system-installed-version")
}

// ── fresh install ─────────────────────────────────────────────────────────

#[test]
fn fresh_install_creates_bundled_skills_and_marker() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();

    for skill in BUNDLED_SKILLS {
        assert!(
            skill_file(&tmp, skill.name).exists(),
            "{} SKILL.md should be created",
            skill.name
        );
    }
    assert!(marker_file(&tmp).exists(), "marker should be created");

    let ver = fs::read_to_string(marker_file(&tmp)).unwrap();
    assert_eq!(ver.trim(), BUNDLED_SKILL_VERSION);
}

#[test]
fn bundled_integration_skills_use_current_codewhale_commands_and_paths() {
    for (name, body) in [("mcp-builder", MCP_BUILDER_BODY), ("feishu", FEISHU_BODY)] {
        assert!(
            body.contains("codewhale mcp"),
            "{name} must use the current CLI"
        );
        assert!(
            !body.contains("deepseek mcp"),
            "{name} must not recommend the retired CLI name"
        );
    }
    assert!(SKILL_CREATOR_BODY.contains("<workspace>/.codewhale/skills"));
    assert!(SKILL_CREATOR_BODY.contains("~/.codewhale/skills"));
    assert!(SKILL_INSTALLER_BODY.contains("~/.codewhale/skills"));
    // Bundled skills must name live, model-visible tools. `read_file` is
    // retired and cannot dispatch; `File` is a hidden compatibility alias
    // that never appears in a catalog or in `tool_search`, so `read` is the
    // citable name.
    // The visible `read` has no PDF branch (PDF bytes decode as lossy
    // binary), so the pdf skill must route extraction through installed
    // CLIs instead of teaching `read` for it.
    assert!(!PDF_BODY.contains("the built-in `read` tool"));
    assert!(PDF_BODY.contains("`pdftotext`"));
    for (name, body) in [
        ("pdf", PDF_BODY),
        ("help", HELP_BODY),
        ("delegate", DELEGATE_BODY),
        ("best-of-n", BEST_OF_N_BODY),
    ] {
        assert!(
            !body.contains("read_file") && !body.contains("exec_shell"),
            "{name} must not teach a retired tool name"
        );
    }
}

// Regression (Pinvou #490 phantom-tool class): bundled skill bodies must
// never cite hidden compatibility aliases or retired dispatch-only names.
// Those names never appear in a model-visible catalog or in a `tool_search`
// result, so hosts whose allowlists derive from the wire catalog reject the
// call outright — the backticked `File` citations in pdf/help stalled real
// reasoning loops exactly that way. The list is backtick-anchored so plain
// prose (e.g. "Files or modules") never false-positives. Entries are the
// registry's hidden compatibility aliases (`File`, `Bash`, the todo family,
// `update_plan`, `rlm`) plus the canonical retired-name lists
// (`RETIRED_TOOL_NAMES`, the `agents/*` family); `list_dir` is deliberately
// absent because it is a live, searchable tool (tools/file.rs ListDirTool),
// not a phantom. Scope note: `v4-best-practices` and `feishu` are not in
// BUNDLED_SKILLS (legacy/optional, never auto-installed), so this sweep does
// not cover them.
const PHANTOM_NAMES: &[&str] = &[
    "`File`",
    "`Bash`",
    "`exec_shell`",
    "`exec_shell_wait`",
    "`exec_shell_interact`",
    "`exec_shell_cancel`",
    "`read_file`",
    "`write_file`",
    "`edit_file`",
    "`fetch_url`",
    "`web_fetch`",
    "`web_search`",
    "`work_update`",
    "`TodoWrite`",
    "`todo`",
    "`checklist_write`",
    "`checklist_update`",
    "`update_plan`",
    "`rlm`",
    "`run_tests`",
    "`run_verifiers`",
    "`git_status`",
    "`git_diff`",
    "`git_log`",
    "`git_show`",
    "`git_blame`",
    "`wait_for_dev_server`",
    "`agents/list`",
    "`agents/message`",
    "`agents/coordinate`",
    "`agents/followup`",
    "`agents/interrupt`",
    "`agents/wait`",
];

#[test]
fn forkguard_bundled_skills_cite_no_hidden_or_retired_tool_names() {
    for skill in BUNDLED_SKILLS {
        for phantom in PHANTOM_NAMES {
            assert!(
                !skill.body.contains(phantom),
                "bundled skill `{}` cites {phantom}, which no model-visible \
                 catalog or `tool_search` result can ever return:\n{}",
                skill.name,
                skill.body
            );
        }
    }
}

// The denylist above is a hand-maintained superset of the canonical lists in
// `tools/canonical_action.rs` (`RETIRED_TOOL_NAMES`, `HIDDEN_COMPAT_TOOL_NAMES`)
// plus extra fork-era names (the todo family, `rlm`, the `agents/*` set). A
// newly registered hidden alias would otherwise skip this sweep silently, so
// anchor the hand list to the canonical one: every canonical entry must stay
// covered here in its backticked citation form.
#[test]
fn forkguard_phantom_denylist_covers_canonical_lists() {
    let canonical = crate::tools::canonical_action::RETIRED_TOOL_NAMES
        .iter()
        .chain(crate::tools::canonical_action::HIDDEN_COMPAT_TOOL_NAMES);
    for name in canonical {
        let cited = format!("`{name}`");
        assert!(
            PHANTOM_NAMES.contains(&cited.as_str()),
            "PHANTOM_NAMES must cover canonical entry {cited} or retired/hidden \
             names can re-enter bundled skill bodies unguarded"
        );
    }
}

// Regression: `create_goal` is deferred-but-`tool_search`-searchable in main
// sessions and removed from subagent registries entirely (tools/subagent/
// mod.rs drops it before catalog filtering and search), so the best-of-n body
// must gate the command on availability, name the activation path instead of
// surrendering a reachable capability, and stay honest for child sessions.
#[test]
fn forkguard_best_of_n_goal_tool_is_availability_gated() {
    let skill = BUNDLED_SKILLS
        .iter()
        .find(|skill| skill.name == "best-of-n")
        .expect("best-of-n must be bundled");
    assert!(
        skill.body.contains("`create_goal` is in your tool list"),
        "best-of-n must gate create_goal on availability:\n{}",
        skill.body
    );
    assert!(
        skill.body.contains("run `tool_search` first"),
        "create_goal is deferred on every stock host, so the visibility gate \
         alone would always fail; the skill must name the activation path:\n{}",
        skill.body
    );
    assert!(
        skill.body.contains("does not exist and"),
        "best-of-n must stay honest for subagent sessions where create_goal \
         is removed entirely:\n{}",
        skill.body
    );
    assert!(
        !skill.body.contains("`create_goal` or active `/goal`"),
        "best-of-n must not command create_goal unconditionally:\n{}",
        skill.body
    );
}

/// #4227 (requested by @JayBeest): the contributor sync/gate/digest skill
/// ships in generation 8, and its two load-bearing refusals — never move a
/// contributor's HEAD, never touch a dirty tree — must survive any later
/// edit to the body.
#[test]
fn contributor_onboarding_ships_at_generation_8_and_keeps_its_refusals() {
    let skill = BUNDLED_SKILLS
        .iter()
        .find(|skill| skill.name == "contributor-onboarding")
        .expect("contributor-onboarding must be bundled");
    assert_eq!(skill.introduced_in, 8);
    // The pin tracks the current catalog generation: 9 added handoff,
    // 10 added mcp-discovery (#5238), 11 corrected bundled skill bodies
    // from the model-facing text audit (#112) and refreshes superseded
    // shipped copies.
    assert_eq!(BUNDLED_SKILL_VERSION, "11");

    let body = skill.body;
    assert!(body.contains("invocation: explicit-only"));
    // Read-only by default: sync is proposed, never performed.
    assert!(body.contains("Do not run `git fetch`, `git pull`, `git rebase`"));
    assert!(body.contains("Do not stash, discard, reset, or commit a dirty tree"));
    // The gate is quoted from CI rather than paraphrased, and the digest is
    // built from files rather than generated.
    assert!(body.contains("cargo clippy --workspace --all-targets --all-features --locked"));
    assert!(body.contains(".github/workflows/ci.yml"));
    assert!(body.contains("Do not call a model provider"));
    // Provider neutrality: the dogfood step sends nothing anywhere.
    assert!(body.contains("./target/release/codewhale exec --help"));
    assert!(body.contains("Never select a provider for them"));
    // Contributor credit is part of the skill's own contract.
    assert!(body.contains("@JayBeest"));
}

#[test]
fn fresh_install_skills_parse_for_discovery() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();

    let registry = crate::skills::SkillRegistry::discover(tmp.path());
    assert!(
        registry.warnings().is_empty(),
        "bundled skills should parse cleanly: {:?}",
        registry.warnings()
    );

    for skill in BUNDLED_SKILLS {
        let parsed = registry
            .get(skill.name)
            .unwrap_or_else(|| panic!("{} should be discoverable", skill.name));
        assert!(
            !parsed.description.is_empty(),
            "{} should include model-visible description",
            skill.name
        );
    }
}

#[test]
fn corrupt_marker_is_repaired_without_overwriting_user_skill_body() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();

    let user_body = "user-edited body";
    fs::write(skill_file(&tmp, "delegate"), user_body).unwrap();
    fs::remove_dir_all(skill_dir(&tmp, "skill-creator")).unwrap();
    fs::write(marker_file(&tmp), "not-a-version").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert_eq!(
        fs::read_to_string(skill_file(&tmp, "delegate")).unwrap(),
        user_body
    );
    assert!(skill_file(&tmp, "skill-creator").exists());
    assert_eq!(
        fs::read_to_string(marker_file(&tmp)).unwrap().trim(),
        BUNDLED_SKILL_VERSION
    );
}

#[test]
fn directory_marker_is_replaced_and_user_skills_are_preserved() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();

    let user_body = "user-edited body";
    fs::write(skill_file(&tmp, "delegate"), user_body).unwrap();
    fs::remove_dir_all(skill_dir(&tmp, "skill-creator")).unwrap();
    fs::remove_file(marker_file(&tmp)).unwrap();
    fs::create_dir(marker_file(&tmp)).unwrap();
    fs::write(marker_file(&tmp).join("stale-entry"), "stale").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert_eq!(
        fs::read_to_string(skill_file(&tmp, "delegate")).unwrap(),
        user_body
    );
    assert!(skill_file(&tmp, "skill-creator").exists());
    assert!(marker_file(&tmp).is_file());
    assert_eq!(
        fs::read_to_string(marker_file(&tmp)).unwrap().trim(),
        BUNDLED_SKILL_VERSION
    );
}

#[test]
fn invalid_marker_is_repaired_even_when_no_skill_body_changes() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();
    fs::write(marker_file(&tmp), "").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert_eq!(
        fs::read_to_string(marker_file(&tmp)).unwrap().trim(),
        BUNDLED_SKILL_VERSION
    );
}

#[test]
fn bundled_catalog_has_two_complete_truthful_tiers() {
    for skill in BUNDLED_SKILLS {
        assert!(
            bundled_skill_tier(skill.name).is_some(),
            "{} must have a picker tier",
            skill.name
        );
    }
    assert_eq!(
        bundled_skill_tier("best-of-n"),
        Some(BundledSkillTier::CoreAgentic)
    );
    assert_eq!(
        bundled_skill_tier("pdf"),
        Some(BundledSkillTier::FormatTooling)
    );
    assert_eq!(bundled_skill_tier("user-created"), None);
    assert!(
        !is_bundled_skill_name("imagine"),
        "do not advertise image generation without an image-generation tool"
    );
}

// ── idempotence ───────────────────────────────────────────────────────────

#[test]
fn calling_twice_is_idempotent() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();

    for skill in BUNDLED_SKILLS {
        fs::write(
            skill_file(&tmp, skill.name),
            format!("{}-sentinel", skill.name),
        )
        .unwrap();
    }

    install_system_skills(tmp.path()).unwrap();

    for skill in BUNDLED_SKILLS {
        let body = fs::read_to_string(skill_file(&tmp, skill.name)).unwrap();
        assert_eq!(
            body,
            format!("{}-sentinel", skill.name),
            "second install should not overwrite {}",
            skill.name
        );
    }
}

// ── user deleted a directory ──────────────────────────────────────────────

#[test]
fn user_deleted_dir_is_not_recreated() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();

    // Simulate user deliberately removing one skill directory.
    fs::remove_dir_all(skill_dir(&tmp, "delegate")).unwrap();

    // Re-launch must NOT recreate the deleted directory.
    install_system_skills(tmp.path()).unwrap();

    assert!(
        !skill_file(&tmp, "delegate").exists(),
        "delegate must not be recreated after user deleted it"
    );
    assert!(
        skill_file(&tmp, "skill-creator").exists(),
        "skill-creator should still be present (not deleted by user)"
    );
}

#[test]
fn user_deleted_all_dirs_are_not_recreated() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();

    for skill in BUNDLED_SKILLS {
        fs::remove_dir_all(skill_dir(&tmp, skill.name)).unwrap();
    }

    install_system_skills(tmp.path()).unwrap();

    for skill in BUNDLED_SKILLS {
        assert!(
            !skill_file(&tmp, skill.name).exists(),
            "{} must not be recreated after user deletion",
            skill.name
        );
    }
}

// ── version bump re-installs ──────────────────────────────────────────────

#[test]
fn outdated_marker_triggers_reinstall_of_existing_skills() {
    let tmp = TempDir::new().unwrap();
    // Exact shipped bodies present with old marker: refresh is allowed and
    // newer skills are added. Non-matching user content is preserved
    // elsewhere (see upgrade_preserves_user_modified_bundled_skill_body).
    for skill in BUNDLED_SKILLS.iter().filter(|s| s.introduced_in <= 4) {
        fs::create_dir_all(skill_dir(&tmp, skill.name)).unwrap();
        fs::write(skill_file(&tmp, skill.name), skill.body).unwrap();
    }
    fs::write(marker_file(&tmp), "0").unwrap();

    install_system_skills(tmp.path()).unwrap();

    for skill in BUNDLED_SKILLS {
        assert!(
            skill_file(&tmp, skill.name).exists(),
            "{} should be installed after marker upgrade",
            skill.name
        );
        let content = fs::read_to_string(skill_file(&tmp, skill.name)).unwrap();
        assert_eq!(
            content, skill.body,
            "{} body should match shipped",
            skill.name
        );
    }
    let ver = fs::read_to_string(marker_file(&tmp)).unwrap();
    assert_eq!(ver.trim(), BUNDLED_SKILL_VERSION);
}

// ── partial previous install ─────────────────────────────────────────────

#[test]
fn version_bump_adds_skills_introduced_after_marker() {
    let tmp = TempDir::new().unwrap();
    // Pre-v5 install: only skills introduced through v4, with exact bodies.
    for skill in BUNDLED_SKILLS.iter().filter(|s| s.introduced_in <= 4) {
        fs::create_dir_all(skill_dir(&tmp, skill.name)).unwrap();
        fs::write(skill_file(&tmp, skill.name), skill.body).unwrap();
    }
    fs::write(marker_file(&tmp), "4").unwrap();

    install_system_skills(tmp.path()).unwrap();

    for skill in BUNDLED_SKILLS.iter().filter(|s| s.introduced_in == 5) {
        assert!(
            skill_file(&tmp, skill.name).exists(),
            "v5 skill {} should be installed on upgrade",
            skill.name
        );
    }
    // Unchanged exact bodies remain current.
    for skill in BUNDLED_SKILLS.iter().filter(|s| s.introduced_in <= 4) {
        let content = fs::read_to_string(skill_file(&tmp, skill.name)).unwrap();
        assert_eq!(content, skill.body);
    }
    let ver = fs::read_to_string(marker_file(&tmp)).unwrap();
    assert_eq!(ver.trim(), BUNDLED_SKILL_VERSION);
}

#[test]
fn version_bump_from_v8_adds_handoff_without_recreating_deleted_skills() {
    let tmp = TempDir::new().unwrap();
    fs::write(marker_file(&tmp), "8").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert!(skill_file(&tmp, "handoff").is_file());
    assert!(
        !skill_file(&tmp, "delegate").exists(),
        "an intentionally absent older skill must stay absent"
    );
    assert_eq!(
        fs::read_to_string(marker_file(&tmp)).unwrap().trim(),
        BUNDLED_SKILL_VERSION
    );
}

#[test]
fn version_bump_from_v5_adds_best_of_n_without_recreating_deleted_skills() {
    let tmp = TempDir::new().unwrap();
    fs::write(marker_file(&tmp), "5").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert!(skill_file(&tmp, "best-of-n").is_file());
    assert!(
        !skill_file(&tmp, "delegate").exists(),
        "an intentionally absent older skill must stay absent"
    );
    assert_eq!(
        fs::read_to_string(marker_file(&tmp)).unwrap().trim(),
        BUNDLED_SKILL_VERSION
    );
}

#[test]
fn version_bump_respects_deleted_existing_skill_while_adding_new_skill() {
    let tmp = TempDir::new().unwrap();

    // Simulate v2 where older bundled skills had been deliberately removed
    // before later versions introduced more system skills.
    fs::write(marker_file(&tmp), "2").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert!(
        !skill_file(&tmp, "skill-creator").exists(),
        "version bump should not recreate deleted skill-creator"
    );
    assert!(
        !skill_file(&tmp, "delegate").exists(),
        "version bump should not recreate deleted delegate"
    );
    for skill in BUNDLED_SKILLS
        .iter()
        .filter(|skill| skill.introduced_in > 2)
    {
        assert!(
            skill_file(&tmp, skill.name).exists(),
            "version bump should install newly introduced {}",
            skill.name
        );
    }
    let ver = fs::read_to_string(marker_file(&tmp)).unwrap();
    assert_eq!(ver.trim(), BUNDLED_SKILL_VERSION);
}

// ── upgrade ───────────────────────────────────────────────────────────────

#[test]
fn upgrade_from_v4_installs_pack_and_retires_unchanged_v4_best_practices() {
    let tmp = TempDir::new().unwrap();
    // Simulate a v4 install: marker + legacy skill bodies.
    fs::create_dir_all(skill_dir(&tmp, "v4-best-practices")).unwrap();
    fs::write(
        skill_file(&tmp, "v4-best-practices"),
        V4_BEST_PRACTICES_BODY,
    )
    .unwrap();
    fs::write(marker_file(&tmp), "4").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert!(
        !skill_dir(&tmp, "v4-best-practices").exists(),
        "unchanged v4-best-practices must be retired"
    );
    assert!(skill_file(&tmp, "debug").exists());
    assert!(skill_file(&tmp, "docx").exists());
    assert!(skill_file(&tmp, "release").exists());
    // Feishu is optional — not auto-installed by the default pack.
    assert!(
        !skill_dir(&tmp, "feishu").exists(),
        "feishu must not be universally installed"
    );
    let ver = fs::read_to_string(marker_file(&tmp)).unwrap();
    assert_eq!(ver.trim(), BUNDLED_SKILL_VERSION);
}

#[test]
fn upgrade_preserves_modified_v4_best_practices() {
    let tmp = TempDir::new().unwrap();
    fs::create_dir_all(skill_dir(&tmp, "v4-best-practices")).unwrap();
    fs::write(
        skill_file(&tmp, "v4-best-practices"),
        "---\nname: v4-best-practices\ndescription: user-owned\n---\n\n# mine\n",
    )
    .unwrap();
    fs::write(marker_file(&tmp), "4").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert!(skill_dir(&tmp, "v4-best-practices").exists());
    let body = fs::read_to_string(skill_file(&tmp, "v4-best-practices")).unwrap();
    assert!(
        body.contains("user-owned"),
        "modified body must be preserved"
    );
}

#[test]
fn upgrade_preserves_user_modified_bundled_skill_body() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();
    let path = skill_file(&tmp, "debug");
    fs::write(
        &path,
        "---\nname: debug\ndescription: customized\n---\n\n# custom\n",
    )
    .unwrap();
    // Force version bump attempt
    fs::write(marker_file(&tmp), "4").unwrap();
    install_system_skills(tmp.path()).unwrap();
    let body = fs::read_to_string(path).unwrap();
    assert!(
        body.contains("customized"),
        "user edit must not be overwritten by name alone"
    );
}

// Round-6 review pin: a generation bump must refresh an installed copy whose
// body is a known superseded shipped body — the old shipped text is not a
// user edit, and without this the bump would fossilize the generation-10
// bodies forever (#4691 non-destructive upgrade table). The generation-10
// documents stub body is embedded here because the digest table stores only
// hashes.
#[test]
fn version_bump_refreshes_superseded_shipped_body() {
    #[rustfmt::skip]
    const GENERATION_10_DOCUMENTS: &str = "---\nname: documents\ndescription: Compatibility alias for the docx skill. Prefer loading docx.\naliases-for: docx\n---\n\n# documents (alias)\n\nThis skill is a compatibility alias for **docx**.\n\nLoad and follow the `docx` skill body for the actual workflow.\n";
    let tmp = TempDir::new().unwrap();
    assert!(
        is_superseded_shipped_body("documents", GENERATION_10_DOCUMENTS),
        "embedded generation-10 documents body must match the superseded digest"
    );
    fs::create_dir_all(skill_dir(&tmp, "documents")).unwrap();
    fs::write(skill_file(&tmp, "documents"), GENERATION_10_DOCUMENTS).unwrap();
    fs::write(marker_file(&tmp), "10").unwrap();

    install_system_skills(tmp.path()).unwrap();

    let current = BUNDLED_SKILLS
        .iter()
        .find(|s| s.name == "documents")
        .expect("documents must stay bundled")
        .body;
    assert_eq!(
        fs::read_to_string(skill_file(&tmp, "documents")).unwrap(),
        current,
        "superseded shipped body must be refreshed on bump"
    );
    assert_eq!(fs::read_to_string(marker_file(&tmp)).unwrap().trim(), "11");
}

#[test]
fn version_bump_still_preserves_user_modified_body() {
    let tmp = TempDir::new().unwrap();
    fs::create_dir_all(skill_dir(&tmp, "documents")).unwrap();
    fs::write(
        skill_file(&tmp, "documents"),
        "---\nname: documents\ndescription: customized\n---\n\n# custom\n",
    )
    .unwrap();
    fs::write(marker_file(&tmp), "10").unwrap();

    install_system_skills(tmp.path()).unwrap();

    assert!(
        fs::read_to_string(skill_file(&tmp, "documents"))
            .unwrap()
            .contains("customized"),
        "user edit must not be refreshed by the bump"
    );
}

// The digest table must only reference bundled skills whose current body
// differs from the recorded digest — a stale entry (skill dropped from the
// bundle, or digest equal to the current body) would silently disable the
// refresh or the preserve arm.
#[test]
fn superseded_digests_reference_bundled_skills_with_changed_bodies() {
    use sha2::{Digest, Sha256};
    for (name, digest) in SUPERSEDED_SHIPPED_BODY_DIGESTS {
        let skill = BUNDLED_SKILLS
            .iter()
            .find(|s| s.name == *name)
            .unwrap_or_else(|| panic!("superseded digest references non-bundled skill {name}"));
        let current: String = Sha256::digest(skill.body.as_bytes())
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect();
        assert_ne!(
            &current, digest,
            "{name}: superseded digest equals the current body digest; drop the entry"
        );
    }
}

#[test]
fn end_user_pack_skills_parse_for_discovery() {
    let tmp = TempDir::new().unwrap();
    install_system_skills(tmp.path()).unwrap();
    let registry = crate::skills::SkillRegistry::discover(tmp.path());
    assert!(
        registry.warnings().is_empty(),
        "bundled skills should parse cleanly: {:?}",
        registry.warnings()
    );
    for name in [
        "debug", "test", "review", "document", "docx", "release", "plan", "verify",
    ] {
        assert!(registry.get(name).is_some(), "{name} must be discoverable");
    }
}

#[test]
fn procedural_skill_homes_remain_bundled_and_lazy() {
    for name in ["debug", "best-of-n", "simplify", "verify", "test", "review"] {
        assert!(
            is_bundled_skill_name(name),
            "procedural skill home must remain available on demand: {name}"
        );
    }
}
