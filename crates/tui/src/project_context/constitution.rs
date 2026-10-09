//! `.codewhale/constitution.json` — the Codewhale-specific repo authority and
//! prioritization policy. This module owns discovery (workspace upward to the
//! git root), parsing, the rendered `<codewhale_repo_constitution>` authority
//! block, and the mechanically enforceable write holds compiled for
//! `crate::repo_law`.

use std::path::{Path, PathBuf};

use serde::Deserialize;

use super::{context_candidate_exists, find_git_root, join_relative_components, load_context_file};

/// Relative path (within a workspace or one of its parents) to the
/// Codewhale-specific repo authority/prioritization policy.
const REPO_CONSTITUTION_RELATIVE_PATH: &[&str] = &[".codewhale", "constitution.json"];

/// `schema_version` understood by this build of the constitution loader.
const SUPPORTED_CONSTITUTION_SCHEMA: u32 = 1;

/// Codewhale-specific repo authority/prioritization policy, loaded from
/// `.codewhale/constitution.json`. All fields are optional so a minimal file
/// (or a future schema) still parses; unknown fields are ignored.
#[derive(Debug, Clone, Default, Deserialize)]
struct RepoConstitution {
    #[serde(default)]
    schema_version: Option<u32>,
    /// Ordered list of sources to trust when local sources conflict
    /// (highest authority first).
    #[serde(default)]
    authority: Option<Vec<String>>,
    /// Repo invariants the agent must not break. Plain strings are advisory
    /// prose (rendered into the prompt only); object entries with `paths`
    /// are additionally compiled into mechanical write holds (see
    /// `crate::repo_law`). Law can only tighten — there is no allow shape.
    #[serde(default)]
    protected_invariants: Option<Vec<ProtectedInvariant>>,
    /// Branch / release policy in effect (e.g. "PRs target codex/v0.8.53").
    #[serde(default)]
    branch_policy: Option<String>,
    /// Conditions under which the agent should stop and escalate to the user.
    #[serde(default)]
    escalate_when: Option<Vec<String>>,
    #[serde(default)]
    verification_policy: Option<VerificationPolicy>,
}

#[derive(Debug, Clone, Default, Deserialize)]
struct VerificationPolicy {
    /// Steps to perform before claiming a task is done.
    #[serde(default)]
    before_claiming_done: Option<Vec<String>>,
}

/// One protected invariant: either advisory prose (the historical shape) or
/// an enforced entry carrying path globs. Untagged so existing files keep
/// parsing unchanged.
#[derive(Debug, Clone, Deserialize)]
#[serde(untagged)]
enum ProtectedInvariant {
    Advisory(String),
    Enforced(EnforcedInvariant),
}

#[derive(Debug, Clone, Deserialize)]
struct EnforcedInvariant {
    text: String,
    /// Workspace-relative path globs this invariant protects (e.g.
    /// `crates/protocol/**`). Empty means advisory-only despite the shape.
    #[serde(default)]
    paths: Vec<String>,
    /// What the harness does when a write targets a protected path.
    #[serde(default)]
    action: RepoLawAction,
}

/// Enforcement level for a protected path. `Ask` force-prompts in
/// approval-gated postures and fails closed without a modal in Full Access;
/// `Block` denies outright in every posture.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum RepoLawAction {
    #[default]
    Ask,
    Block,
}

/// A compiled, mechanically-enforceable repo-law rule.
pub(crate) struct RepoLawRule {
    pub(crate) text: String,
    pub(crate) patterns: Vec<String>,
    pub(crate) globs: globset::GlobSet,
    pub(crate) action: RepoLawAction,
}

/// Load and compile the enforceable rules from the workspace's repo
/// constitution. Any failure — missing file, parse error, invalid glob —
/// degrades to fewer (or zero) rules: enforcement can silently do less,
/// never more, and never poisons the tool gate. Parse warnings still reach
/// the user through the prompt-side load path, which reads the same file.
///
/// The read+parse+globset compile is cached per constitution file and
/// revalidated by (mtime, size) on every call (review #484/CodeWhale
/// round-24 B24-5): the repo-law gate runs on every write-tool call, and a
/// 64-root session re-walking git roots and recompiling the same
/// constitution once per root per call converted the gate into a hot loop.
/// The upward discovery walk itself stays uncached so a constitution
/// created at a nearer directory is found on the next call; only the
/// compile of an already-resolved file is memoized.
pub(crate) fn load_repo_law_rules(workspace: &Path) -> std::sync::Arc<Vec<RepoLawRule>> {
    static CACHE: std::sync::OnceLock<
        std::sync::Mutex<std::collections::HashMap<PathBuf, CachedRepoLawRules>>,
    > = std::sync::OnceLock::new();
    let Some(path) = discover_repo_constitution_path(workspace) else {
        return std::sync::Arc::new(Vec::new());
    };
    let Ok(metadata) = std::fs::metadata(&path) else {
        return std::sync::Arc::new(Vec::new());
    };
    let modified = metadata
        .modified()
        .unwrap_or(std::time::SystemTime::UNIX_EPOCH);
    let len = metadata.len();
    let cache = CACHE.get_or_init(|| std::sync::Mutex::new(std::collections::HashMap::new()));
    let mut guard = cache
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    if let Some(hit) = guard.get(&path)
        && hit.modified == modified
        && hit.len == len
    {
        return std::sync::Arc::clone(&hit.rules);
    }
    // Round-31 M31-1: a READ FAILURE is never cached — `chmod 000 ->
    // enforcement trigger -> chmod 644` leaves mtime/len unchanged (chmod
    // only touches ctime), so a cached empty entry would hit forever and
    // repo law stays silent for the process's lifetime (fail-open; the
    // base re-reads every call and self-heals). read_repo_constitution
    // cannot distinguish not-found/parse-failure from a legitimately empty
    // file at its Option boundary, so the discrimination happens here:
    // only a file that READS successfully (load_context_file Ok) enters
    // the cache — including an unparseable-but-readable one, whose
    // empty-rule compile is then honestly keyed to an mtime the repair
    // will change.
    if !path.is_file() {
        return std::sync::Arc::new(Vec::new());
    }
    if load_context_file(&path).is_err() {
        eprintln!("[repo-law] constitution read failed (not cached; next call re-reads)");
        return std::sync::Arc::new(Vec::new());
    }
    let rules = std::sync::Arc::new(
        read_repo_constitution(&path)
            .map(compile_repo_law_rules)
            .unwrap_or_default(),
    );
    guard.insert(
        path,
        CachedRepoLawRules {
            modified,
            len,
            rules: std::sync::Arc::clone(&rules),
        },
    );
    rules
}

struct CachedRepoLawRules {
    modified: std::time::SystemTime,
    len: u64,
    rules: std::sync::Arc<Vec<RepoLawRule>>,
}

/// Compile the enforceable rules from an already-parsed constitution: text
/// and path globs are trimmed, empty or uncompilable entries degrade to
/// fewer rules, and only entries carrying usable globs become holds.
fn compile_repo_law_rules(constitution: RepoConstitution) -> Vec<RepoLawRule> {
    let mut rules = Vec::new();
    for invariant in constitution.protected_invariants.into_iter().flatten() {
        let ProtectedInvariant::Enforced(enforced) = invariant else {
            continue;
        };
        if enforced.text.trim().is_empty() {
            continue;
        }
        let mut builder = globset::GlobSetBuilder::new();
        let mut patterns = Vec::new();
        for pattern in &enforced.paths {
            let trimmed = pattern.trim();
            if trimmed.is_empty() {
                continue;
            }
            if let Ok(glob) = globset::Glob::new(trimmed) {
                builder.add(glob);
                patterns.push(trimmed.to_string());
            }
        }
        if patterns.is_empty() {
            continue;
        }
        let Ok(globs) = builder.build() else {
            continue;
        };
        rules.push(RepoLawRule {
            text: enforced.text.trim().to_string(),
            patterns,
            globs,
            action: enforced.action,
        });
    }
    rules
}

/// Walk from `workspace` toward the git root looking for the repo
/// constitution; existence-only, no read. The enforcement loader caches the
/// compile behind the returned path.
fn discover_repo_constitution_path(workspace: &Path) -> Option<PathBuf> {
    let git_root = find_git_root(workspace);
    let mut current = workspace.to_path_buf();
    loop {
        let mut path = current.clone();
        for component in REPO_CONSTITUTION_RELATIVE_PATH {
            path.push(component);
        }
        if context_candidate_exists(&path) {
            return Some(path);
        }
        if let Some(ref root) = git_root
            && current == *root
        {
            break;
        }
        match current.parent() {
            Some(parent) if parent != current => current = parent.to_path_buf(),
            _ => break,
        }
    }
    None
}

/// Read and parse the repo constitution at `path`, best-effort: any read or
/// parse failure degrades to None, exactly like the old inline walk.
fn read_repo_constitution(path: &Path) -> Option<RepoConstitution> {
    load_context_file(path)
        .ok()
        .and_then(|raw| serde_json::from_str::<RepoConstitution>(&raw).ok())
}

impl RepoConstitution {
    /// True when the file carried no usable policy (so we can skip emitting an
    /// empty block).
    fn is_empty(&self) -> bool {
        let list_empty = |l: &Option<Vec<String>>| l.as_ref().is_none_or(Vec::is_empty);
        list_empty(&self.authority)
            && self.protected_invariants.as_ref().is_none_or(Vec::is_empty)
            && list_empty(&self.escalate_when)
            && self
                .branch_policy
                .as_ref()
                .is_none_or(|s| s.trim().is_empty())
            && self
                .verification_policy
                .as_ref()
                .and_then(|p| p.before_claiming_done.as_ref())
                .is_none_or(Vec::is_empty)
    }

    /// Render a model-facing authority block (concise prose, per the layered
    /// model: base myth → global constitution → repo constitution = local law).
    fn render_block(&self, source: &Path) -> String {
        let mut body = String::new();
        if let Some(authority) = self.authority.as_ref().filter(|a| !a.is_empty()) {
            body.push_str(
                "When local sources conflict, trust them in this order (highest first):\n",
            );
            for (idx, item) in authority.iter().enumerate() {
                body.push_str(&format!("{}. {item}\n", idx + 1));
            }
        }
        if let Some(invariants) = self.protected_invariants.as_ref().filter(|i| !i.is_empty()) {
            body.push_str("\nProtected invariants — do not break:\n");
            for item in invariants {
                match item {
                    ProtectedInvariant::Advisory(text) => {
                        body.push_str(&format!("- {text}\n"));
                    }
                    ProtectedInvariant::Enforced(enforced) => {
                        let paths = enforced
                            .paths
                            .iter()
                            .map(String::as_str)
                            .collect::<Vec<_>>()
                            .join(", ");
                        if paths.is_empty() {
                            body.push_str(&format!("- {}\n", enforced.text));
                        } else {
                            body.push_str(&format!(
                                "- {} (mechanically enforced for: {paths})\n",
                                enforced.text
                            ));
                        }
                    }
                }
            }
        }
        if let Some(policy) = self.branch_policy.as_ref().filter(|s| !s.trim().is_empty()) {
            body.push_str(&format!("\nBranch / release policy: {}\n", policy.trim()));
        }
        if let Some(steps) = self
            .verification_policy
            .as_ref()
            .and_then(|p| p.before_claiming_done.as_ref())
            .filter(|s| !s.is_empty())
        {
            body.push_str("\nBefore claiming a task is done:\n");
            for step in steps {
                body.push_str(&format!("- {step}\n"));
            }
        }
        if let Some(conditions) = self.escalate_when.as_ref().filter(|c| !c.is_empty()) {
            body.push_str("\nStop and escalate to the user when:\n");
            for item in conditions {
                body.push_str(&format!("- {item}\n"));
            }
        }
        format!(
            "<codewhale_repo_constitution source=\"{}\">\nCodewhale-specific repo authority policy (local law: subordinate to the global Constitution and the current user request, but above memory and old handoffs; WHALE.md is ignored and should be migrated, not treated as law).\n\n{}</codewhale_repo_constitution>",
            // Same origin-label convention as `<project_instructions>`: file
            // name only. The rendered `source` here is a runtime-canonicalized
            // absolute path (workspace-relative traversal from `REPO_CONSTITUTION_RELATIVE_PATH`),
            // but its final segment is that compile-time constant, so the
            // base name is stable across directory moves and recasings. This
            // keeps absolute paths out of provider-bound prompt labels.
            // Operators still get the locator via `constitution_source_path`
            // in the report and `/constitution`.
            super::project_instructions_source_label(Some(source)),
            body.trim_end()
        )
    }

    fn policy_warnings(&self, source: &Path) -> Vec<String> {
        let mut warnings = Vec::new();
        if let Some(policy) = self.branch_policy.as_deref()
            && branch_policy_looks_stale(policy)
        {
            warnings.push(format!(
                "{} branch_policy appears stale: hard-coded release branch guidance (`{}`). Use live branch/handoff truth and AGENTS.md instead of versioned integration-lane text.",
                source.display(),
                policy.trim()
            ));
        }
        warnings
    }
}

fn branch_policy_looks_stale(policy: &str) -> bool {
    let lower = policy.to_ascii_lowercase();
    lower.contains("codex/v")
        || ((lower.contains("integration branch") || lower.contains("not main"))
            && contains_release_version_token(policy))
}

fn contains_release_version_token(value: &str) -> bool {
    value
        .split(|ch: char| !(ch.is_ascii_alphanumeric() || ch == '.'))
        .any(|token| {
            let token = token.trim_start_matches(['v', 'V']);
            let mut parts = token.split('.');
            matches!(
                (parts.next(), parts.next(), parts.next(), parts.next()),
                (Some(major), Some(minor), Some(patch), None)
                    if major.chars().all(|ch| ch.is_ascii_digit())
                        && minor.chars().all(|ch| ch.is_ascii_digit())
                        && patch.chars().all(|ch| ch.is_ascii_digit())
            )
        })
}

/// Discover and render `.codewhale/constitution.json` from `workspace` or, if
/// absent, its parent directories up to the git root. Returns the rendered
/// authority block plus any parse warnings.
pub(crate) fn load_repo_constitution_block(
    workspace: &Path,
) -> (Option<String>, Option<PathBuf>, Vec<String>) {
    let mut warnings = Vec::new();
    let git_root = find_git_root(workspace);
    let mut current = workspace.to_path_buf();
    loop {
        let mut path = current.clone();
        for component in REPO_CONSTITUTION_RELATIVE_PATH {
            path.push(component);
        }
        if context_candidate_exists(&path) {
            match load_context_file(&path) {
                Ok(raw) => match serde_json::from_str::<RepoConstitution>(&raw) {
                    Ok(constitution) if !constitution.is_empty() => {
                        if let Some(version) = constitution.schema_version
                            && version != SUPPORTED_CONSTITUTION_SCHEMA
                        {
                            warnings.push(format!(
                                "{} declares schema_version {version}; this build supports {SUPPORTED_CONSTITUTION_SCHEMA}. Reading it on a best-effort basis.",
                                path.display()
                            ));
                        }
                        warnings.extend(constitution.policy_warnings(&path));
                        return (Some(constitution.render_block(&path)), Some(path), warnings);
                    }
                    Ok(_) => {
                        warnings.push(format!(
                            "{} has no authority/verification policy; ignoring.",
                            path.display()
                        ));
                        return (None, None, warnings);
                    }
                    Err(e) => {
                        warnings.push(format!("Failed to parse {}: {e}", path.display()));
                        return (None, None, warnings);
                    }
                },
                Err(e) => {
                    warnings.push(format!("Failed to read {}: {e}", path.display()));
                    return (None, None, warnings);
                }
            }
        }
        if let Some(ref root) = git_root
            && current == *root
        {
            break;
        }
        match current.parent() {
            Some(parent) if parent != current => current = parent.to_path_buf(),
            _ => break,
        }
    }
    (None, None, warnings)
}

pub(crate) fn repo_constitution_candidate_paths(workspace: &Path) -> Vec<PathBuf> {
    let git_root = find_git_root(workspace);
    let mut current = workspace.to_path_buf();
    let mut paths = Vec::new();
    loop {
        paths.push(join_relative_components(
            &current,
            REPO_CONSTITUTION_RELATIVE_PATH,
        ));
        if let Some(ref root) = git_root
            && current == *root
        {
            break;
        }
        match current.parent() {
            Some(parent) if parent != current => current = parent.to_path_buf(),
            _ => break,
        }
    }
    paths
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use tempfile::tempdir;

    #[test]
    fn mixed_advisory_and_enforced_invariants_render_and_back_compat_holds() {
        let tmp = tempdir().expect("tempdir");
        let dir = tmp.path().join(".codewhale");
        fs::create_dir_all(&dir).expect("law dir");
        fs::write(
            dir.join("constitution.json"),
            r#"{
                "protected_invariants": [
                    "Plain advisory prose.",
                    { "text": "The wire format is frozen", "paths": ["crates/protocol/**"], "action": "block" }
                ]
            }"#,
        )
        .expect("write law");

        let (block, path, warnings) = load_repo_constitution_block(tmp.path());
        let block = block.expect("law renders");
        assert!(path.is_some());
        assert!(warnings.is_empty(), "{warnings:?}");
        assert!(block.contains("- Plain advisory prose."), "{block}");
        assert!(
            block.contains(
                "- The wire format is frozen (mechanically enforced for: crates/protocol/**)"
            ),
            "{block}"
        );

        // The enforcement loader compiles only the enforced entry.
        let rules = load_repo_law_rules(tmp.path());
        assert_eq!(rules.len(), 1);
        assert_eq!(rules[0].text, "The wire format is frozen");
        assert_eq!(rules[0].action, RepoLawAction::Block);
        assert!(rules[0].globs.is_match("crates/protocol/wire.rs"));
    }

    #[test]
    fn legacy_string_only_invariants_render_unchanged_and_compile_nothing() {
        let tmp = tempdir().expect("tempdir");
        let dir = tmp.path().join(".codewhale");
        fs::create_dir_all(&dir).expect("law dir");
        fs::write(
            dir.join("constitution.json"),
            r#"{"protected_invariants": ["Keep DeepSeek support first-class."]}"#,
        )
        .expect("write law");

        let (block, _, warnings) = load_repo_constitution_block(tmp.path());
        let block = block.expect("law renders");
        assert!(warnings.is_empty(), "{warnings:?}");
        assert!(
            block.contains("- Keep DeepSeek support first-class."),
            "{block}"
        );
        assert!(!block.contains("mechanically enforced"), "{block}");
        assert!(load_repo_law_rules(tmp.path()).is_empty());
    }

    #[test]
    fn repository_constitution_avoids_hard_coded_release_lane_policy() {
        let repo_constitution = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../..")
            .join(".codewhale")
            .join("constitution.json");
        let raw = fs::read_to_string(&repo_constitution).expect("read repo constitution");
        let constitution: RepoConstitution =
            serde_json::from_str(&raw).expect("parse repo constitution");
        let warnings = constitution.policy_warnings(&repo_constitution);
        assert!(
            warnings.is_empty(),
            "repo constitution should not carry stale release-lane policy: {:?}",
            warnings
        );
    }
    /// Round-32 (M31-1 pin): a READ failure is never cached — the chmod-000
    /// window self-heals on the next read once permissions recover (chmod
    /// only touches ctime, so mtime/len stay identical), and the empty
    /// answer never becomes sticky. Drives the public cache entry with the
    /// discoverable `.codewhale/constitution.json` placement.
    #[test]
    fn constitution_read_failure_is_not_cached_across_permission_recovery() {
        let dir = tempfile::tempdir().expect("tempdir");
        let nested = dir.path().join("work");
        std::fs::create_dir_all(nested.join(".codewhale")).unwrap();
        let path = nested.join(".codewhale").join("constitution.json");
        std::fs::write(
            &path,
            r#"{"protected_invariants": [{"text": "stay kind", "paths": ["src/**"]}]}"#,
        )
        .expect("seed");

        // The failure window comes FIRST (nothing cached yet) — the cache
        // is a process-global keyed on the resolved path, so a prior good
        // call for the same path would legitimately serve its cached rules
        // and mask the pin.
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o000)).unwrap();
            let degraded = load_repo_law_rules(&nested);
            let read_blocked = std::fs::read(&path).is_err();
            if read_blocked {
                assert!(
                    degraded.is_empty(),
                    "the read failure answers empty for this call"
                );
            }
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
            let recovered = load_repo_law_rules(&nested);
            assert!(
                !recovered.is_empty(),
                "recovery is not blocked by a cached empty entry (mtime/len unchanged by chmod)"
            );
        }
        let readable = load_repo_law_rules(&nested);
        assert!(
            !readable.is_empty(),
            "the readable constitution compiles to at least one rule"
        );
    }
}
