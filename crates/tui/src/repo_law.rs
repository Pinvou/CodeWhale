//! Mechanical enforcement of repo-law protected invariants.
//!
//! `.codewhale/constitution.json` invariants were previously advisory prose
//! rendered into the prompt. Entries that carry `paths` globs now also
//! compile into write holds evaluated in the engine's tool gate — the law
//! becomes mechanism, with a receipt naming the invariant.
//!
//! The contract mirrors the project-overlay rule ("overrides may only
//! tighten"):
//!
//! - Law can only ADD holds. There is no allow/widen shape in the schema, so
//!   a crafted constitution cannot grant authority.
//! - `ask` force-prompts only in Ask posture. Auto-Review, Full Access, and
//!   Never never open tool-approval prompts, so the same law fails closed
//!   there. `block` denies outright in every posture.
//! - Any failure (missing file, parse error, bad glob) degrades to fewer or
//!   zero rules — never a poisoned gate, never a hold on unprotected paths.
//! - Only the repo-local constitution participates. The user-global
//!   constitution stays advisory prose and never reaches this module.

use std::path::{Component, Path, PathBuf};

use serde_json::Value;

use crate::project_context::{RepoLawAction, load_repo_law_rules};
use crate::tools::apply_patch::{NormalizedApplyPatchInput, normalize_apply_patch_input};

/// Semantic write actions whose inputs name filesystem targets we can hold.
/// Canonical action families are resolved to this policy vocabulary before the
/// check, so removing callable compatibility aliases cannot open a law bypass.
const WRITE_POLICY_ACTIONS: &[&str] = &["write_file", "edit_file", "apply_patch", "fim_edit"];

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum RepoLawPlanDecision {
    /// Request a policy-forced approval naming the law. The engine converts
    /// this to a hard block in non-interactive Full Access.
    ForcePrompt(String),
    /// Deny the call outright, naming the law.
    Block(String),
}

/// Evaluate the accessible roots' repo law against a proposed tool call.
///
/// `workspace_roots` carries the roots attached beside the primary; an empty
/// set (a host that never materialized one) keeps exactly the single-root
/// behavior. Each root is judged in its own namespace because the globs are
/// workspace-relative and two roots can carry different laws, so a root's
/// constitution holds the writes that land under it.
///
/// A *relative* target is judged against every root, not only the one
/// execution will resolve it under: `push_normalized` keeps the relative tail
/// per root, so an attached root's law can hold a write that execution would
/// place under the primary. That direction is deliberate and fail-closed —
/// the worst case is an extra prompt or block, never a missed hold — and
/// `strongest_hold_wins_across_roots` pins it. Returns `None` for tools
/// without write targets, roots without enforceable law, and writes outside
/// every protected glob.
pub(crate) fn repo_law_plan_decision(
    workspace: &Path,
    workspace_roots: &[PathBuf],
    tool_name: &str,
    tool_input: &Value,
) -> Option<RepoLawPlanDecision> {
    if crate::prompts::static_prompt_composer_installed() {
        // The embedding host owns the reviewed authority channel; an ambient
        // constitution must not add a hidden mechanical-policy source.
        return None;
    }
    let policy_action =
        crate::tools::canonical_action::canonical_action_alias(tool_name, tool_input);
    if !WRITE_POLICY_ACTIONS.contains(&policy_action) {
        return None;
    }

    // Strongest action wins across all (root, rule, target) matches. The
    // reason is built where the match is found so the borrow does not have to
    // outlive the per-root rule set.
    let mut hold: Option<(bool, String)> = None;
    for root in codewhale_core::normalize_workspace_roots(workspace, workspace_roots) {
        let targets = write_target_paths(workspace, &root, tool_input);
        if targets.is_empty() {
            continue;
        }
        let rules = load_repo_law_rules(&root);
        for rule in &rules {
            for target in &targets {
                if !rule.globs.is_match(target) {
                    continue;
                }
                let blocking = matches!(rule.action, RepoLawAction::Block);
                let already_blocking = hold.as_ref().is_some_and(|(blocking, _)| *blocking);
                if (blocking || hold.is_none()) && !already_blocking {
                    hold = Some((
                        blocking,
                        format!(
                            "Repo law holds this write: \"{}\" protects {} (matched {target}, .codewhale/constitution.json)",
                            rule.text,
                            rule.patterns.join(", ")
                        ),
                    ));
                }
            }
        }
    }
    let (blocking, reason) = hold?;
    Some(if blocking {
        RepoLawPlanDecision::Block(reason)
    } else {
        RepoLawPlanDecision::ForcePrompt(reason)
    })
}

/// Extract workspace-relative write targets from a tool input. Covers the
/// `path`/`target`/`destination`/`file_path` params, canonical
/// `replace[].path`, legacy `changes[].path`, and
/// every unified-diff / codex-envelope header shape the patch tools accept —
/// old (`--- `) and new (`+++ `) paths, with or without an `a/`/`b/` prefix,
/// tab-timestamp suffixes stripped, and `/dev/null` (deletion) falling back
/// to the counterpart path. Missing any shape the tool honors is a hold
/// bypass, so this deliberately over-collects candidate paths.
///
/// `workspace` is the primary root (what execution resolves relative targets
/// against); `root` is the root whose law is being judged.
fn write_target_paths(workspace: &Path, root: &Path, input: &Value) -> Vec<String> {
    let mut targets = Vec::new();
    for key in ["path", "target", "destination", "file_path"] {
        if let Some(path) = input.get(key).and_then(Value::as_str) {
            push_normalized(&mut targets, workspace, root, path);
        }
    }
    match normalize_apply_patch_input(input) {
        Ok(NormalizedApplyPatchInput::Replacement { entries, .. }) => {
            for change in entries {
                if let Some(path) = change.get("path").and_then(Value::as_str) {
                    push_normalized(&mut targets, workspace, root, path);
                }
            }
        }
        Ok(NormalizedApplyPatchInput::Patch(patch)) => {
            let mut pending_old: Option<String> = None;
            for line in patch.lines() {
                if let Some(rest) = line.strip_prefix("*** Update File: ") {
                    push_normalized(&mut targets, workspace, root, rest.trim());
                } else if let Some(rest) = line.strip_prefix("*** Add File: ") {
                    push_normalized(&mut targets, workspace, root, rest.trim());
                } else if let Some(rest) = line.strip_prefix("*** Delete File: ") {
                    push_normalized(&mut targets, workspace, root, rest.trim());
                } else if let Some(rest) = line.strip_prefix("--- ") {
                    // Old path: remember it so a `+++ /dev/null` deletion still
                    // holds the file being removed.
                    pending_old = diff_header_path(rest);
                    if let Some(ref p) = pending_old {
                        push_normalized(&mut targets, workspace, root, p);
                    }
                } else if let Some(rest) = line.strip_prefix("+++ ") {
                    match diff_header_path(rest) {
                        Some(new_path) => push_normalized(&mut targets, workspace, root, &new_path),
                        // `+++ /dev/null` → deletion; the target is the old path.
                        None => {
                            if let Some(old) = pending_old.take() {
                                push_normalized(&mut targets, workspace, root, &old);
                            }
                        }
                    }
                }
            }
        }
        Err(_) => {}
    }
    targets.sort();
    targets.dedup();
    targets
}

/// Parse a unified-diff header path: strip an optional `a/`/`b/` prefix and a
/// tab-delimited timestamp suffix. Returns `None` for `/dev/null` (absence).
fn diff_header_path(rest: &str) -> Option<String> {
    // Headers may carry a "\t<timestamp>" suffix; the path is the first field.
    let path = rest.split('\t').next().unwrap_or(rest).trim();
    if path.is_empty() || path == "/dev/null" {
        return None;
    }
    let stripped = path
        .strip_prefix("a/")
        .or_else(|| path.strip_prefix("b/"))
        .unwrap_or(path);
    Some(stripped.to_string())
}

/// Normalize to a forward-slash, root-relative string so globs written
/// as `crates/x/**` match regardless of how the tool spelled the path. Crucially
/// this collapses `.`/`..` path components the same way the write tools'
/// `resolve_path` does, so an interior `crates/./protocol/x` or
/// `x/../crates/protocol/x` cannot spell its way past a glob (a confirmed
/// bypass before this).
///
/// `workspace` (primary) and `root` (the law being judged) differ for
/// multi-root sessions. A relative spelling is judged against every root —
/// keep the raw collapsed tail per root, so an attached root's law can hold
/// a write that execution would place under the primary (fail-closed: an
/// extra prompt or block at worst). When that collapse leaves a leading
/// `..` marker — a `..`-spelled target execution resolves *outside* the
/// spelling's own root — also judge the execution-resolved path: join it
/// onto the primary, collapse lexically (what `resolve_path` normalizes to),
/// and keep its tail under *this* root when it lands inside. Without that,
/// `../attached/secret/x` keeps its `..` spelling in the attached root's
/// tail and that root's anchored globs never fire — a spelling-only bypass
/// of the attached law.
fn push_normalized(targets: &mut Vec<String>, workspace: &Path, root: &Path, raw: &str) {
    let trimmed = raw.trim().replace('\\', "/");
    if trimmed.is_empty() {
        return;
    }
    // Make root-relative when the tool gave an absolute path inside it.
    let path = Path::new(&trimmed);
    let relative = path.strip_prefix(root).unwrap_or(path);

    // Lexically collapse CurDir (`.`) and ParentDir (`..`) components, and
    // drop any leading root/empty component. An absolute path outside the
    // workspace keeps its tail (e.g. `/etc/passwd` -> `etc/passwd`) so a
    // `**/passwd` glob still matches while a workspace-anchored glob does not.
    let mut parts: Vec<String> = Vec::new();
    for component in relative.to_string_lossy().split('/') {
        match component {
            "" | "." => {}
            ".." => {
                // A `..` that pops above the root escapes the workspace; keep
                // an explicit marker so it can never match a workspace-relative
                // glob, and the ordinary approval/sandbox gates still govern it.
                if parts.pop().is_none() {
                    parts.push("..".to_string());
                }
            }
            other => parts.push(other.to_string()),
        }
    }
    // A surviving leading `..` is a relative spelling that escapes this root.
    // Where execution actually lands it (primary-joined, lexically collapsed)
    // may still be inside *this* root — judge that shape too, so the root's
    // anchored globs hold it. The raw spelling above is kept unchanged, so
    // the fail-closed across-roots judgment for plain relative targets is
    // untouched.
    if parts.first().map(String::as_str) == Some("..") && !path.is_absolute() {
        // Execution joins the *raw* spelling onto the primary and lexically
        // normalizes it (`ToolContext::resolve_path`). Derive that path with
        // component operations — splitting display strings is not a path
        // operation and silently misparses Windows separators — and keep its
        // tail under this root when it lands inside.
        if let Some(candidate) = normalize_lexical_components(&workspace.join(raw))
            && let Ok(tail) = candidate.strip_prefix(root)
        {
            let tail = tail.to_string_lossy().replace('\\', "/");
            if !tail.is_empty() {
                targets.push(tail);
            }
        }
    }
    let normalized = parts.join("/");
    if !normalized.is_empty() {
        targets.push(normalized);
    }
}

/// Lexically collapse CurDir and ParentDir components of `path` (what the
/// write tools' `resolve_path` normalizes a joined candidate to), using
/// component operations so the result is platform-correct. `None` when a
/// `..` escapes above the filesystem root: there is no sane tail to judge
/// against any root, and the ordinary gates govern the call.
fn normalize_lexical_components(path: &Path) -> Option<PathBuf> {
    let mut out = PathBuf::new();
    for component in path.components() {
        match component {
            Component::CurDir => {}
            Component::ParentDir => {
                if !out.pop() {
                    return None;
                }
            }
            component => out.push(component.as_os_str()),
        }
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use tempfile::TempDir;

    fn write_law(workspace: &Path, body: &str) {
        let dir = workspace.join(".codewhale");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("constitution.json"), body).unwrap();
    }

    /// Single-root call, the shape every host without a materialized root set
    /// uses.
    fn decide(
        workspace: &Path,
        tool_name: &str,
        tool_input: &Value,
    ) -> Option<RepoLawPlanDecision> {
        repo_law_plan_decision(workspace, &[], tool_name, tool_input)
    }

    const LAW: &str = r#"{
        "authority": ["AGENTS.md"],
        "protected_invariants": [
            "Keep DeepSeek support first-class.",
            { "text": "The wire format is frozen", "paths": ["crates/protocol/**"], "action": "block" },
            { "text": "Release notes need human review", "paths": ["CHANGELOG.md"] }
        ]
    }"#;

    #[test]
    fn advisory_only_law_never_holds() {
        let tmp = TempDir::new().unwrap();
        write_law(
            tmp.path(),
            r#"{"protected_invariants": ["Prose only, no paths."]}"#,
        );
        assert_eq!(
            decide(
                tmp.path(),
                "write_file",
                &json!({"path": "src/main.rs", "content": "x"}),
            ),
            None
        );
    }

    #[test]
    fn block_action_denies_protected_write() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);
        let decision = decide(
            tmp.path(),
            "write_file",
            &json!({"path": "crates/protocol/wire.rs", "content": "x"}),
        );
        let Some(RepoLawPlanDecision::Block(reason)) = decision else {
            panic!("expected block, got {decision:?}");
        };
        assert!(reason.contains("The wire format is frozen"), "{reason}");
        assert!(reason.contains("crates/protocol/wire.rs"), "{reason}");
        assert!(reason.contains(".codewhale/constitution.json"), "{reason}");
    }

    #[test]
    fn ask_action_force_prompts_and_names_the_law() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);
        let decision = decide(
            tmp.path(),
            "edit_file",
            &json!({"path": "CHANGELOG.md", "old": "a", "new": "b"}),
        );
        let Some(RepoLawPlanDecision::ForcePrompt(reason)) = decision else {
            panic!("expected force prompt, got {decision:?}");
        };
        assert!(
            reason.contains("Release notes need human review"),
            "{reason}"
        );
    }

    #[test]
    fn canonical_file_write_and_edit_actions_receive_the_same_holds() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);

        let blocked = decide(
            tmp.path(),
            "File",
            &json!({
                "action": "write",
                "path": "crates/protocol/wire.rs",
                "content": "x"
            }),
        );
        assert!(matches!(blocked, Some(RepoLawPlanDecision::Block(_))));

        let held = decide(
            tmp.path(),
            "File",
            &json!({
                "action": "edit",
                "path": "CHANGELOG.md",
                "search": "before",
                "replace": "after"
            }),
        );
        assert!(matches!(held, Some(RepoLawPlanDecision::ForcePrompt(_))));
    }

    #[test]
    fn unprotected_writes_and_non_write_tools_pass() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);
        assert_eq!(
            decide(
                tmp.path(),
                "write_file",
                &json!({"path": "src/main.rs", "content": "x"}),
            ),
            None
        );
        assert_eq!(
            decide(
                tmp.path(),
                "read_file",
                &json!({"path": "crates/protocol/wire.rs"}),
            ),
            None
        );
    }

    #[test]
    fn apply_patch_targets_are_extracted_from_all_shapes() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);
        // Canonical replace[].path shape.
        let decision = decide(
            tmp.path(),
            "apply_patch",
            &json!({"replace": [{"path": "crates/protocol/msg.rs"}]}),
        );
        assert!(matches!(decision, Some(RepoLawPlanDecision::Block(_))));
        // Legacy changes[].path shape must receive the same hold.
        let decision = decide(
            tmp.path(),
            "apply_patch",
            &json!({"changes": [{"path": "crates/protocol/msg.rs"}]}),
        );
        assert!(matches!(decision, Some(RepoLawPlanDecision::Block(_))));
        // unified diff shape
        let decision = decide(
            tmp.path(),
            "apply_patch",
            &json!({"patch": "--- a/crates/protocol/msg.rs\n+++ b/crates/protocol/msg.rs\n@@\n"}),
        );
        assert!(matches!(decision, Some(RepoLawPlanDecision::Block(_))));
        // codex envelope shape
        let decision = decide(
            tmp.path(),
            "apply_patch",
            &json!({"patch": "*** Begin Patch\n*** Update File: crates/protocol/msg.rs\n*** End Patch\n"}),
        );
        assert!(matches!(decision, Some(RepoLawPlanDecision::Block(_))));
    }

    #[test]
    fn block_outranks_ask_when_both_match() {
        let tmp = TempDir::new().unwrap();
        write_law(
            tmp.path(),
            r#"{"protected_invariants": [
                { "text": "ask first", "paths": ["docs/**"] },
                { "text": "never", "paths": ["docs/frozen/**"], "action": "block" }
            ]}"#,
        );
        let decision = decide(
            tmp.path(),
            "write_file",
            &json!({"path": "docs/frozen/spec.md", "content": "x"}),
        );
        assert!(matches!(decision, Some(RepoLawPlanDecision::Block(_))));
    }

    #[test]
    fn absolute_and_dot_prefixed_paths_normalize_to_workspace_relative() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);
        let absolute = tmp.path().join("crates/protocol/wire.rs");
        let decision = decide(
            tmp.path(),
            "write_file",
            &json!({"path": absolute.to_string_lossy(), "content": "x"}),
        );
        assert!(matches!(decision, Some(RepoLawPlanDecision::Block(_))));
        let decision = decide(
            tmp.path(),
            "write_file",
            &json!({"path": "./CHANGELOG.md", "content": "x"}),
        );
        assert!(matches!(
            decision,
            Some(RepoLawPlanDecision::ForcePrompt(_))
        ));
    }

    #[test]
    fn malformed_law_and_bad_globs_degrade_to_no_holds() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), "{ not json");
        assert_eq!(
            decide(
                tmp.path(),
                "write_file",
                &json!({"path": "crates/protocol/wire.rs", "content": "x"}),
            ),
            None
        );
        write_law(
            tmp.path(),
            r#"{"protected_invariants": [
                { "text": "broken glob", "paths": ["crates/[invalid"] }
            ]}"#,
        );
        assert_eq!(
            decide(
                tmp.path(),
                "write_file",
                &json!({"path": "crates/protocol/wire.rs", "content": "x"}),
            ),
            None
        );
    }

    #[test]
    fn interior_dot_and_parent_segments_cannot_evade_a_block() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);
        for path in [
            "crates/./protocol/wire.rs",
            "crates/../crates/protocol/wire.rs",
            "x/../crates/protocol/wire.rs",
            "./crates/protocol/wire.rs",
        ] {
            let decision = decide(
                tmp.path(),
                "write_file",
                &json!({ "path": path, "content": "x" }),
            );
            assert!(
                matches!(decision, Some(RepoLawPlanDecision::Block(_))),
                "{path} must be held, got {decision:?}"
            );
        }
    }

    #[test]
    fn fim_edit_is_gated_like_other_write_tools() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);
        let decision = decide(
            tmp.path(),
            "fim_edit",
            &json!({ "path": "crates/protocol/wire.rs", "prefix": "a", "suffix": "b" }),
        );
        assert!(
            matches!(decision, Some(RepoLawPlanDecision::Block(_))),
            "{decision:?}"
        );
    }

    #[test]
    fn apply_patch_header_variants_are_all_extracted() {
        let tmp = TempDir::new().unwrap();
        write_law(tmp.path(), LAW);
        // no a/ or b/ prefix
        let d = decide(
            tmp.path(),
            "apply_patch",
            &json!({ "patch": "--- crates/protocol/wire.rs\n+++ crates/protocol/wire.rs\n@@\n" }),
        );
        assert!(
            matches!(d, Some(RepoLawPlanDecision::Block(_))),
            "no-prefix: {d:?}"
        );
        // deletion: +++ /dev/null, target is the old path
        let d = decide(
            tmp.path(),
            "apply_patch",
            &json!({ "patch": "--- a/crates/protocol/wire.rs\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n" }),
        );
        assert!(
            matches!(d, Some(RepoLawPlanDecision::Block(_))),
            "deletion: {d:?}"
        );
        // tab-timestamp suffix on the header
        let d = decide(
            tmp.path(),
            "apply_patch",
            &json!({ "patch": "--- a/x\t2026-01-01\n+++ b/crates/protocol/wire.rs\t2026-01-01 10:00:00\n@@\n" }),
        );
        assert!(
            matches!(d, Some(RepoLawPlanDecision::Block(_))),
            "tab-timestamp: {d:?}"
        );
    }

    #[test]
    fn no_law_file_means_no_holds() {
        let tmp = TempDir::new().unwrap();
        assert_eq!(
            decide(
                tmp.path(),
                "write_file",
                &json!({"path": "anything.rs", "content": "x"}),
            ),
            None
        );
    }

    #[test]
    fn attached_root_law_holds_writes_under_that_root() {
        let primary = TempDir::new().unwrap();
        let attached = TempDir::new().unwrap();
        write_law(primary.path(), LAW);
        write_law(
            attached.path(),
            r#"{"protected_invariants": [
                { "text": "Vendored tree is read-only", "paths": ["vendor/**"], "action": "block" }
            ]}"#,
        );
        let roots = vec![attached.path().to_path_buf()];

        // Root-relative globs stay root-relative: the primary's
        // `crates/protocol/**` must not fire on a same-shaped path under an
        // attached root just because the tail happens to look alike.
        assert_eq!(
            repo_law_plan_decision(
                primary.path(),
                &roots,
                "write_file",
                &json!({
                    "path": attached.path().join("crates/protocol/wire.rs"),
                    "content": "x"
                }),
            ),
            None
        );

        let decision = repo_law_plan_decision(
            primary.path(),
            &roots,
            "write_file",
            &json!({"path": attached.path().join("vendor/lib.rs"), "content": "x"}),
        );
        let Some(RepoLawPlanDecision::Block(reason)) = decision else {
            panic!("expected the attached root's law to block, got {decision:?}");
        };
        assert!(reason.contains("Vendored tree is read-only"), "{reason}");
    }

    #[test]
    fn strongest_hold_wins_across_roots() {
        let primary = TempDir::new().unwrap();
        let attached = TempDir::new().unwrap();
        write_law(
            primary.path(),
            r#"{"protected_invariants": [
                { "text": "ask in primary", "paths": ["shared/**"] }
            ]}"#,
        );
        write_law(
            attached.path(),
            r#"{"protected_invariants": [
                { "text": "block in attached", "paths": ["shared/**"], "action": "block" }
            ]}"#,
        );

        // The same relative target matches an Ask in the primary and a Block
        // in the attached root; law can only add holds, so the Block wins.
        let decision = repo_law_plan_decision(
            primary.path(),
            &[attached.path().to_path_buf()],
            "write_file",
            &json!({"path": "shared/lib.rs", "content": "x"}),
        );
        assert!(
            matches!(decision, Some(RepoLawPlanDecision::Block(_))),
            "{decision:?}"
        );
    }

    #[test]
    fn dotdot_spelled_relative_target_still_holds_in_the_attached_root() {
        // A `..`-spelled relative target execution resolves into an attached
        // root must not escape that root's anchored globs by spelling: the
        // judged tail is the execution-resolved path under the containing
        // root, not the raw `..` spelling (which never matched anything).
        let primary = TempDir::new().unwrap();
        let attached = TempDir::new().unwrap();
        write_law(
            attached.path(),
            r#"{"protected_invariants": [
                { "text": "Vendored tree is read-only", "paths": ["vendor/**"], "action": "block" }
            ]}"#,
        );
        let spelling = format!(
            "../{}/vendor/lib.rs",
            attached.path().file_name().unwrap().to_string_lossy()
        );

        let decision = repo_law_plan_decision(
            primary.path(),
            &[attached.path().to_path_buf()],
            "write_file",
            &json!({"path": spelling, "content": "x"}),
        );
        let Some(RepoLawPlanDecision::Block(reason)) = decision else {
            panic!(
                "expected the attached root's law to hold a ..-spelled target, got {decision:?}"
            );
        };
        assert!(reason.contains("Vendored tree is read-only"), "{reason}");

        // A `..`-spelled target that resolves outside every root stays
        // unheld by anchored globs (the ordinary gates govern it).
        let decision = repo_law_plan_decision(
            primary.path(),
            &[attached.path().to_path_buf()],
            "write_file",
            &json!({"path": "../../elsewhere/lib.rs", "content": "x"}),
        );
        assert_eq!(decision, None, "an out-of-tree escape has no anchored hold");
    }
}
