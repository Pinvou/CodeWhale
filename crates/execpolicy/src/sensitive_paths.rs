//! Canonical sensitive-path inventory and the builtin safety ruleset.
//!
//! This module replaces the sensitive-path inventories embedders otherwise
//! hand-maintain in parallel: the motivating embedder kept the same
//! credential/system path list in three places (an ingest-gate component
//! blocklist, a generated hard-deny ruleset, and the hook both migrated
//! from) with live drift between them. The union of those lists lives here
//! once, and [`builtin_safety_ruleset`] derives policy from it so an embedder
//! stops maintaining its own copy.
//!
//! ## The containment gap this module fills
//!
//! The deny-rule token channel matches whole command tokens exactly: a rule
//! can name `~/.ssh`, but no rule can say "any path UNDER `~/.ssh`", because
//! `~/.ssh/config` is a different token. [`contains`] is that missing
//! containment primitive for callers that hold real paths (ingest gates,
//! file-tool checks). On the token channel itself the gap remains: today's
//! embedders paper over it by generating a corpus of thousands of enumerated
//! rules. That corpus exists precisely because matching lacks containment —
//! it collapses into this inventory once rule evaluation gains containment
//! matching. Until then [`builtin_safety_ruleset`] anchors only the
//! exact-token faces of the inventory.
//!
//! ## What the builtin ruleset deliberately is not
//!
//! It is hard deny (`denied_prefixes`), and deny always wins across layers —
//! a user- or agent-layer ruleset cannot un-deny it. That is the right
//! posture for system credentials (`cat /etc/shadow` has no legitimate agent
//! reading), but it also denies the key-rotation vocabulary (`cp new_key
//! ~/.ssh/authorized_keys`). Embedders that need those allowances keep them
//! outside this ruleset rather than weakening the inventory.

use std::path::Path;

use crate::{
    Ruleset, RulesetLayer, parse_path_for_matching_with_case, platform_paths_are_case_insensitive,
};

/// Directory names whose contents are credentials or session material.
///
/// Home-relative spellings; an entry may itself be nested (`.config/gcloud`).
/// A path with any of these as a component is sensitive — see [`contains`]
/// for the component-boundary check.
pub const SENSITIVE_DIRECTORY_NAMES: &[&str] = &[
    // OpenSSH home: private keys, authorized_keys, config.
    ".ssh",
    // GnuPG home: secret keyrings and ownertrust.
    ".gnupg",
    // Modern GnuPG secret-key store (gpg >= 2.1 keeps keys here, not secring.gpg).
    ".gnupg/private-keys-v1.d",
    // AWS CLI: shared credentials file and config.
    ".aws",
    // gcloud: application-default credentials and its SQLite credential store.
    ".config/gcloud",
    // Azure CLI: MSAL token caches.
    ".azure",
    // Docker CLI: registry auth in config.json.
    ".docker",
    // Kubernetes: cluster admission credentials in config.
    ".kube",
    // Chrome profile: cookie and login databases.
    ".config/google-chrome",
    // Firefox profile: logins.json protected by key4.db.
    ".mozilla/firefox",
    // pass(1) password store.
    ".password-store",
    // Application credential stores carried from the motivating embedder's inventory.
    ".dws",
    ".tmeet",
];

/// Files whose content is itself a credential, as home-relative paths.
///
/// Entries under a directory name from [`SENSITIVE_DIRECTORY_NAMES`] are that
/// directory's well-known children; bare entries live at the home root. The
/// last two are sensitive as a path component ANYWHERE (they are the ingest
/// gate's component face) and are listed at their home-root spelling here.
pub const SENSITIVE_FILE_NAMES: &[&str] = &[
    // OpenSSH private keys, one per algorithm.
    ".ssh/id_rsa",
    ".ssh/id_ed25519",
    ".ssh/id_ecdsa",
    ".ssh/id_dsa",
    // SSH trust anchors: writing here grants future access.
    ".ssh/authorized_keys",
    // Per-host OpenSSH settings (carries key locations and ProxyCommands).
    ".ssh/config",
    // Cluster admission credentials.
    ".kube/config",
    // Docker registry auth blob.
    ".docker/config.json",
    // AWS CLI settings and long-lived credentials.
    ".aws/config",
    ".aws/credentials",
    // gcloud ADC token file and its SQLite credential store.
    ".config/gcloud/application_default_credentials.json",
    ".config/gcloud/credentials.db",
    // Azure CLI MSAL token cache, current and legacy spellings.
    ".azure/msal_token_cache.json",
    ".azure/accessTokens.json",
    // Chrome cookie database (session hijacking).
    ".config/google-chrome/Default/Cookies",
    // Chrome saved logins.
    ".config/google-chrome/Default/Login Data",
    // Chrome master key protecting every credential above.
    ".config/google-chrome/Local State",
    // Legacy GnuPG secret keyring.
    ".gnupg/secring.gpg",
    // Home-root secret files.
    "credentials",
    "secrets",
    ".pgp",
    ".gpg",
    // Auto-login credentials read by curl/ftp.
    ".netrc",
    // git credential-store plaintext.
    ".git-credentials",
    // Sensitive as a path component anywhere (ingest gate); home-root spelling.
    "credentials.json",
    ".env",
];

/// Absolute path prefixes outside any home that must not be touched.
///
/// An entry ending in `/` is a directory prefix (its whole subtree is
/// sensitive); `/var/log/auth` is a slash-less prefix that also catches the
/// `auth.log` rotation spellings on the ingest face.
pub const SENSITIVE_ABSOLUTE_PREFIXES: &[&str] = &[
    // Login password hashes; "-" and ".bak" are the editor/aging backups.
    "/etc/shadow",
    "/etc/shadow-",
    "/etc/shadow.bak",
    // Group password hashes (same root-only exposure as shadow).
    "/etc/gshadow",
    "/etc/gshadow-",
    // sudo policy; "-" and ".bak" backups, plus the arbitrary-named fragments directory.
    "/etc/sudoers",
    "/etc/sudoers-",
    "/etc/sudoers.bak",
    "/etc/sudoers.d/",
    // Host keys and sshd configuration.
    "/etc/ssh/",
    // The root account's whole home is privileged ground.
    "/root/",
    // Auth logs, including the .log rotations via the prefix face.
    "/var/log/auth",
    // Virtual filesystems exposing process env and kernel state; never a workspace.
    "/proc/",
    "/sys/",
];

/// Sensitive directory names, home-relative (may be nested).
pub fn sensitive_directory_names() -> &'static [&'static str] {
    SENSITIVE_DIRECTORY_NAMES
}

/// Sensitive credential files, as home-relative paths.
pub fn sensitive_file_names() -> &'static [&'static str] {
    SENSITIVE_FILE_NAMES
}

/// Sensitive absolute path prefixes outside any home.
pub fn sensitive_absolute_prefixes() -> &'static [&'static str] {
    SENSITIVE_ABSOLUTE_PREFIXES
}

/// True when `candidate` lies inside `dir` at a strict component boundary.
///
/// `/home/u` does NOT contain `/home/user/...` — the boundary is a whole
/// path component, never a string prefix. A path contains itself (equal
/// paths return true), and trailing separators are irrelevant.
///
/// This is the primitive the deny-rule token channel lacks: a token rule can
/// name `~/.ssh` exactly, but cannot express "anything under `~/.ssh`".
/// Callers that hold real paths (ingest gates, file-tool checks) use this to
/// get the containment face the token channel cannot spell.
///
/// The comparison is purely lexical, matching the crate's rule-matching
/// philosophy: no symlink resolution and no filesystem access, so callers
/// comparing real files should canonicalize first. Separators fold (`\` to
/// `/`), `.` components collapse, and case folds only on platforms whose
/// filesystems fold case (Windows, default macOS) — consistent with
/// [`crate::normalize_workspace_relative_path`]. A `..` component, an empty
/// path, or a drive-relative Windows path is rejected outright rather than
/// resolved, so containment fails closed on anything ambiguous. A relative
/// `dir` and an absolute `candidate` (or vice versa) never contain one
/// another.
pub fn contains(dir: &Path, candidate: &Path) -> bool {
    contains_with_case(dir, candidate, platform_paths_are_case_insensitive())
}

fn contains_with_case(dir: &Path, candidate: &Path, case_insensitive: bool) -> bool {
    let (Some(dir), Some(candidate)) = (
        parse_path_for_matching_with_case(&dir.to_string_lossy(), case_insensitive),
        parse_path_for_matching_with_case(&candidate.to_string_lossy(), case_insensitive),
    ) else {
        return false;
    };
    match (&dir.root, &candidate.root) {
        (Some(dir_root), Some(candidate_root)) if dir_root == candidate_root => {}
        // Two relative paths compare by components alone; a relative and an
        // absolute path cannot contain one another.
        (None, None) => {}
        _ => return false,
    }
    candidate.components.starts_with(&dir.components[..])
}

/// Builds the builtin-default safety ruleset from the inventory.
///
/// This is the first producer of [`RulesetLayer::BuiltinDefault`] — the layer
/// existed so hosts could differentiate engine-provided policy from their own
/// agent/user layers, but nothing populated it. The returned ruleset carries
/// only hard denies: for every inventory path it denies ANY command that
/// names the path as a positional token (`* <path>` uses the engine's middle
/// wildcard to anchor on the tail), covering read, destroy, copy, and upload
/// faces alike. `cat /etc/shadow` and `rm -rf ~/.ssh` are denied under every
/// approval mode, including `Never`.
///
/// Only exact-token faces are expressible on the token channel: directory
/// entries get their bare, trailing-slash (`rm -rf /etc/ssh/` is a different
/// token), and glob (`rm /etc/ssh/*` is a literal token a model writes)
/// spellings, but a descendant like `/etc/ssh/sshd_config` still needs the
/// generated-corpus enumeration embedders carry today — or [`contains`] at a
/// call site that holds real paths. Home-relative entries are emitted only in
/// the `~/` spelling; `$HOME`-style, Windows, and resolved-home spellings are
/// further enumeration the same containment gap forces. Both enumerations
/// collapse once rule evaluation gains containment matching.
pub fn builtin_safety_ruleset() -> Ruleset {
    let mut denied_prefixes = Vec::new();
    for prefix in SENSITIVE_ABSOLUTE_PREFIXES {
        let base = prefix.trim_end_matches('/');
        denied_prefixes.push(format!("* {base}"));
        if prefix.ends_with('/') {
            denied_prefixes.push(format!("* {prefix}"));
            denied_prefixes.push(format!("* {prefix}*"));
        }
    }
    for dir in SENSITIVE_DIRECTORY_NAMES {
        denied_prefixes.push(format!("* ~/{dir}"));
        denied_prefixes.push(format!("* ~/{dir}/"));
        denied_prefixes.push(format!("* ~/{dir}/*"));
    }
    for file in SENSITIVE_FILE_NAMES {
        denied_prefixes.push(format!("* ~/{file}"));
    }
    denied_prefixes.sort();
    denied_prefixes.dedup();
    Ruleset {
        layer: RulesetLayer::BuiltinDefault,
        trusted_prefixes: Vec::new(),
        denied_prefixes,
        ask_rules: Vec::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{AskForApproval, ExecApprovalRequirement, ExecPolicyContext, ExecPolicyEngine};

    fn ctx(command: &str) -> ExecPolicyContext<'_> {
        ExecPolicyContext {
            command,
            cwd: "/workspace",
            tool: Some("exec_shell"),
            path: None,
            ask_for_approval: AskForApproval::Never,
            sandbox_mode: Some("workspace-write"),
        }
    }

    #[test]
    fn inventory_is_non_empty_deduped_and_spotted() {
        for (name, list) in [
            ("directory names", sensitive_directory_names()),
            ("file names", sensitive_file_names()),
            ("absolute prefixes", sensitive_absolute_prefixes()),
        ] {
            assert!(!list.is_empty(), "{name} inventory is empty");
            let mut sorted = list.to_vec();
            sorted.sort();
            sorted.dedup();
            assert_eq!(sorted.len(), list.len(), "{name} inventory has duplicates");
        }
        // Spot vectors from the two embedder inventories this union carries.
        assert!(sensitive_directory_names().contains(&".ssh"));
        assert!(sensitive_directory_names().contains(&".config/gcloud"));
        assert!(sensitive_file_names().contains(&".ssh/id_rsa"));
        assert!(sensitive_file_names().contains(&".env"));
        assert!(sensitive_absolute_prefixes().contains(&"/etc/shadow"));
        assert!(sensitive_absolute_prefixes().contains(&"/proc/"));
    }

    #[test]
    fn contains_respects_component_boundaries() {
        let cases: &[(&str, &str, bool)] = &[
            // The boundary case: `u` is a different component than `user`.
            ("/home/u", "/home/user/secrets", false),
            ("/home/u", "/home/u/notes", true),
            // A path contains itself.
            ("/home/u", "/home/u", true),
            ("/home/u/", "/home/u/x", true),
            ("/home/u", "/home", false),
            ("/etc/ssh", "/etc/ssh/sshd_config", true),
            // `..` is rejected rather than collapsed: fail closed.
            ("/home/u", "/home/u/../u2/x", false),
            ("..", "/home/u", false),
            // Empty and mixed relative/absolute forms match nothing.
            ("/home/u", "", false),
            ("/home/u", ".ssh/id_rsa", false),
            (".ssh", "/home/u/.ssh/id_rsa", false),
            // Relative-to-relative compares by components alone.
            (".ssh", ".ssh/id_rsa", true),
            (".ssh", ".subdir/x", false),
            // Windows drive paths normalize identically on every host.
            ("C:/Users/u", "C:/Users/u/notes", true),
            ("C:/Users/u", "C:/Users/ursula/x", false),
            ("C:\\Users\\u", "C:\\Users\\u\\notes.txt", true),
            // Case folds only where the platform's filesystem folds case.
            (
                "/etc/SSH",
                "/etc/ssh/x",
                platform_paths_are_case_insensitive(),
            ),
        ];
        for (dir, candidate, expected) in cases {
            assert_eq!(
                contains(Path::new(dir), Path::new(candidate)),
                *expected,
                "contains({dir:?}, {candidate:?})"
            );
        }
    }

    #[test]
    fn builtin_ruleset_targets_the_builtin_layer_and_denies_representative_commands() {
        let ruleset = builtin_safety_ruleset();
        assert_eq!(ruleset.layer, RulesetLayer::BuiltinDefault);
        assert!(ruleset.trusted_prefixes.is_empty());
        assert!(ruleset.ask_rules.is_empty());
        assert!(!ruleset.denied_prefixes.is_empty());

        let engine = ExecPolicyEngine::with_rulesets(vec![ruleset]);
        for command in [
            "cat /etc/shadow",
            "sudo cat /etc/shadow",
            "rm -rf ~/.ssh",
            "rm -rf ~/.ssh/",
            "cp /etc/shadow /tmp/exfil",
            "grep root /etc/shadow",
            // Deny matching must survive a benign leading segment.
            "ls && cat /etc/shadow",
        ] {
            let decision = engine.check(ctx(command)).unwrap();
            assert!(
                !decision.allow
                    && matches!(
                        decision.requirement,
                        ExecApprovalRequirement::Forbidden { .. }
                    ),
                "{command} must be hard-denied: {decision:?}"
            );
        }
        // The deny matcher lowercases, like every other prefix rule.
        let decision = engine.check(ctx("CAT /ETC/SHADOW")).unwrap();
        assert!(!decision.allow, "case-spelled sensitive path: {decision:?}");
    }

    #[test]
    fn builtin_ruleset_leaves_innocuous_commands_alone() {
        let engine = ExecPolicyEngine::with_rulesets(vec![builtin_safety_ruleset()]);
        // Under `Never` an unmatched command runs with no prompt at all, so
        // "allowed" here really means the deny scan found nothing.
        for command in ["ls /tmp", "echo hello", "cat README.md", "git status"] {
            let decision = engine.check(ctx(command)).unwrap();
            assert!(
                decision.allow && !decision.requires_approval,
                "{command} must run cleanly: {decision:?}"
            );
        }
    }
}
