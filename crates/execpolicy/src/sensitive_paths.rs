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
//! Scope: this is the union of those three embedder inventories, not of every
//! sensitive-path list in the tree. `crates/tui/src/sandbox/read_guard.rs`
//! keeps a deliberately broader sandbox-face inventory (`.pgpass`, `.npmrc`,
//! macOS keychains, …) behind its own containment helper; consolidating that
//! one means migrating its consumer, so until then it stays the in-tree
//! predecessor this module names but does not replace.
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
//! no trusted prefix, typed allow, or ask rule anywhere else can un-deny it.
//! That is the right posture for system credentials (`cat /etc/shadow` has no
//! legitimate agent reading), but adopters should know it also denies, with
//! no recourse short of filtering `denied_prefixes` before install or not
//! installing: the key-rotation vocabulary (`cp new_key
//! ~/.ssh/authorized_keys`), agent-operated config reads (`kubectl
//! --kubeconfig ~/.kube/config get pods`, `cat ~/.docker/config.json`), and
//! even textual mentions (`grep "cat /etc/shadow" notes.txt` — the expander
//! strips quotes before matching). Allowances live outside this ruleset by
//! construction, not by configuration.

use std::path::Path;

use crate::{Ruleset, parse_path_for_matching_with_case, platform_paths_are_case_insensitive};

/// Directory names whose contents are credentials or session material.
///
/// Home-relative spellings; an entry may itself be nested (`.config/gcloud`).
/// A path with any of these as a component is sensitive — see [`contains`]
/// for the component-boundary check. The single-component entries also form
/// the directory half of [`SENSITIVE_COMPONENT_NAMES`], the component-anywhere
/// face for callers that cannot resolve the home root.
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
    // Application credential stores contributed by the motivating embedder
    // (Pinvou-specific); split out to embedder-supplied input when this
    // module is proposed upstream, per the fork's upstream-hygiene scan.
    ".dws",
    ".tmeet",
];

/// Files whose content is itself a credential, as home-relative paths.
///
/// Entries under a directory name from [`SENSITIVE_DIRECTORY_NAMES`] are that
/// directory's well-known children; bare entries live at the home root. The
/// last two are also members of [`SENSITIVE_COMPONENT_NAMES`] — the ingest
/// gate's component-anywhere face — listed here at their home-root spelling.
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
    // Sensitive as a path component anywhere (see SENSITIVE_COMPONENT_NAMES);
    // home-root spelling.
    "credentials.json",
    ".env",
];

/// Path components that make a path sensitive wherever they appear — the
/// ingest gate's component face, carried in full.
///
/// Unlike [`SENSITIVE_FILE_NAMES`], whose entries are home-root spellings, an
/// entry here matches any single path component (`~/keys/id_rsa`,
/// `/srv/work/.ssh`, `/media/usb/.env`), independent of where the home is.
/// Directories are carried at their bare name so a nested checkout is caught
/// too. See [`has_sensitive_component`] for the check.
pub const SENSITIVE_COMPONENT_NAMES: &[&str] = &[
    // Bare directory names: the component faces of SENSITIVE_DIRECTORY_NAMES.
    ".ssh",
    ".gnupg",
    ".aws",
    ".docker",
    ".kube",
    ".password-store",
    // SSH private-key basenames: keys live wherever the user keeps them, so
    // the ingest gate matches the basename anywhere, not just under ~/.ssh.
    "id_rsa",
    "id_ed25519",
    "id_ecdsa",
    "id_dsa",
    // Credential file names sensitive as a component anywhere.
    "credentials.json",
    ".env",
];

/// Absolute path prefixes outside any home that must not be touched.
///
/// An entry ending in `/` is a directory prefix (its whole subtree is
/// sensitive). A slash-less entry is exact-token on this crate's channels —
/// the source ingest gate's string-prefix behavior does not carry over,
/// which is why the primary `auth.log` spelling is enumerated explicitly.
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
    // Auth logs: the base spelling from the source inventories plus the
    // primary Debian/Ubuntu rotation target; higher rotations (`auth.log.N`)
    // remain enumeration on the token channel.
    "/var/log/auth",
    "/var/log/auth.log",
    // Virtual filesystems exposing process env and kernel state; never a workspace.
    "/proc/",
    "/sys/",
];

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
///
/// Windows verbatim forms are folded before comparison — `\\?\C:\Users\u`
/// loses its `\\?\` prefix and `\\?\UNC\server\share` becomes
/// `\\server\share` — so a `std::fs::canonicalize` result and its plain
/// spelling compare equal. What lexical comparison still cannot see: Win32
/// strips trailing dots and spaces and resolves 8.3 short names at the
/// filesystem layer, and only ASCII case folds. Spell both sides from the
/// same normalization (canonicalize both) when this answer guards a deny.
/// Two residual over-block edges, deliberate: a `dir` that normalizes to no
/// components (`.`) contains every candidate of the same rootedness, and
/// non-UTF-8 bytes fold to U+FFFD before comparison, so two distinct such
/// paths can collide.
#[must_use]
pub fn contains(dir: &Path, candidate: &Path) -> bool {
    contains_with_case(dir, candidate, platform_paths_are_case_insensitive())
}

/// Strips a Windows verbatim prefix so a `std::fs::canonicalize` result and
/// its plain spelling parse to the same path.
fn strip_verbatim_prefix(raw: &str) -> std::borrow::Cow<'_, str> {
    let Some(rest) = raw.strip_prefix(r"\\?\") else {
        return raw.into();
    };
    match rest.strip_prefix(r"UNC\") {
        Some(unc) => format!(r"\\{unc}").into(),
        None => rest.into(),
    }
}

fn contains_with_case(dir: &Path, candidate: &Path, case_insensitive: bool) -> bool {
    let (Some(dir), Some(candidate)) = (
        parse_path_for_matching_with_case(
            &strip_verbatim_prefix(&dir.to_string_lossy()),
            case_insensitive,
        ),
        parse_path_for_matching_with_case(
            &strip_verbatim_prefix(&candidate.to_string_lossy()),
            case_insensitive,
        ),
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

/// True when any single component of `path` is in [`SENSITIVE_COMPONENT_NAMES`].
///
/// This is the ingest gate's component face in full: it catches
/// `~/keys/id_rsa` and a nested `/srv/work/.ssh` that the home-rooted
/// spellings cannot express, wherever the home happens to be. The comparison
/// folds separators and (on case-folding platforms) case via the crate's
/// path parsing, and rejects traversal, empty, and drive-relative paths
/// outright — a caller that cannot affirm sensitivity for an ambiguous path
/// must reject that path on its own.
#[must_use]
pub fn has_sensitive_component(path: &Path) -> bool {
    has_sensitive_component_with_case(path, platform_paths_are_case_insensitive())
}

fn has_sensitive_component_with_case(path: &Path, case_insensitive: bool) -> bool {
    let Some(parsed) = parse_path_for_matching_with_case(
        &strip_verbatim_prefix(&path.to_string_lossy()),
        case_insensitive,
    ) else {
        return false;
    };
    parsed
        .components
        .iter()
        .any(|component| SENSITIVE_COMPONENT_NAMES.contains(&component.as_str()))
}

/// Builds the builtin-default safety ruleset from the inventory.
///
/// This is the first producer of [`crate::RulesetLayer::BuiltinDefault`] —
/// the layer existed so hosts could differentiate engine-provided policy from
/// their own agent/user layers, but nothing populated it. The returned
/// ruleset carries only hard denies: for every inventory path it denies ANY
/// command that names the path as a positional token (`* <path>` uses the
/// engine's middle wildcard to anchor on the tail) — the read face
/// (`cat /etc/shadow`), the destroy face (`rm -rf ~/.ssh`), and the
/// copy/upload faces that take the path as a bare positional argument
/// (`cp /etc/shadow /tmp/exfil`) alike. `cat /etc/shadow` and `rm -rf ~/.ssh`
/// are denied under every approval mode, including `Never`.
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
///
/// The token channel also keeps the engine's raw-token matching, so three
/// bypass families stay open here: kernel-normalized spellings of the same
/// path (`cat /etc//shadow`, `cat /etc/./shadow`, `cat /etc/ssh/../shadow`
/// are different tokens), paths glued into a larger token (`dd
/// if=/etc/shadow`, `curl -F file=@/etc/shadow`), and remote specs
/// (`scp host:~/.ssh/id_rsa .`). The crate's path parsing folds the first
/// family, so callers holding real paths via [`contains`] are covered;
/// closing any of them on the token channel is rule-evaluation work, not
/// inventory work.
///
/// Install the result with [`crate::ExecPolicyEngine::with_rulesets`] or
/// `add_ruleset`; `set_ruleset` replaces a whole layer, so a second
/// BuiltinDefault producer installing over this one would evict it wholesale.
#[must_use]
pub fn builtin_safety_ruleset() -> Ruleset {
    let mut denied_prefixes = Vec::new();
    for prefix in SENSITIVE_ABSOLUTE_PREFIXES {
        let base = prefix.trim_end_matches('/');
        debug_assert!(
            !base.is_empty(),
            "inventory entry {prefix:?} normalizes to no path"
        );
        if base.is_empty() {
            // A wildcard-only rule would deny every command; skipping is the
            // safe release-mode behavior if an entry ever regresses.
            continue;
        }
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
        denied_prefixes,
        ..Ruleset::builtin_default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        AskForApproval, ExecApprovalRequirement, ExecPolicyContext, ExecPolicyEngine,
        PermissionAction, RulesetLayer, ToolAskRule,
    };

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
            ("directory names", SENSITIVE_DIRECTORY_NAMES),
            ("file names", SENSITIVE_FILE_NAMES),
            ("component names", SENSITIVE_COMPONENT_NAMES),
            ("absolute prefixes", SENSITIVE_ABSOLUTE_PREFIXES),
        ] {
            assert!(!list.is_empty(), "{name} inventory is empty");
            let mut sorted = list.to_vec();
            sorted.sort();
            sorted.dedup();
            assert_eq!(sorted.len(), list.len(), "{name} inventory has duplicates");
        }
        // Spot vectors from the two embedder inventories this union carries.
        assert!(SENSITIVE_DIRECTORY_NAMES.contains(&".ssh"));
        assert!(SENSITIVE_DIRECTORY_NAMES.contains(&".config/gcloud"));
        assert!(SENSITIVE_FILE_NAMES.contains(&".ssh/id_rsa"));
        assert!(SENSITIVE_FILE_NAMES.contains(&".env"));
        assert!(SENSITIVE_ABSOLUTE_PREFIXES.contains(&"/etc/shadow"));
        assert!(SENSITIVE_ABSOLUTE_PREFIXES.contains(&"/proc/"));
        assert!(SENSITIVE_ABSOLUTE_PREFIXES.contains(&"/var/log/auth.log"));
        // The component face is the ingest gate's component blocklist.
        assert!(SENSITIVE_COMPONENT_NAMES.contains(&"id_rsa"));
        assert!(SENSITIVE_COMPONENT_NAMES.contains(&".password-store"));
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
            // The mixed-form guard holds even when the spellings align.
            (".ssh", "/.ssh/id_rsa", false),
            // Relative-to-relative compares by components alone.
            (".ssh", ".ssh/id_rsa", true),
            (".ssh", ".subdir/x", false),
            // Windows drive paths normalize identically on every host.
            ("C:/Users/u", "C:/Users/u/notes", true),
            ("C:/Users/u", "C:/Users/ursula/x", false),
            ("C:\\Users\\u", "C:\\Users\\u\\notes.txt", true),
            // Windows verbatim forms fold to their plain spellings, so a
            // canonicalized path and its plain spelling compare equal.
            ("\\\\?\\C:\\Users\\u", "C:\\Users\\u\\notes", true),
            ("C:\\Users\\u", "\\\\?\\C:\\Users\\u\\notes.txt", true),
            ("\\\\?\\UNC\\server\\share", "\\\\server\\share\\x", true),
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
        // The folding flag drives the fold on every host — pin both arms so
        // a case-insensitive regression cannot hide behind a Linux CI run.
        assert!(contains_with_case(
            Path::new("/etc/SSH"),
            Path::new("/etc/ssh/x"),
            true
        ));
        assert!(!contains_with_case(
            Path::new("/etc/SSH"),
            Path::new("/etc/ssh/x"),
            false
        ));
    }

    #[test]
    fn sensitive_components_match_anywhere_in_a_path() {
        // The face the ingest gate carries and the home-rooted spellings in
        // SENSITIVE_FILE_NAMES cannot express.
        let cases: &[(&str, bool)] = &[
            ("~/keys/id_rsa", true),
            ("/srv/work/.ssh/config", true),
            ("~/notes/credentials.json", true),
            ("id_rsa", true),
            // Whole-component equality only.
            ("~/keys/id_rsa_backup", false),
            ("~/notes/id_ring", false),
            // Absolute-prefix faces cover /etc/shadow, not the component face.
            ("/etc/shadow", false),
            // Traversal fails closed, like contains().
            ("~/.ssh/../id_rsa", false),
            ("", false),
        ];
        for (path, expected) in cases {
            assert_eq!(
                has_sensitive_component(Path::new(path)),
                *expected,
                "has_sensitive_component({path:?})"
            );
        }
        // The component face folds case where the platform folds case.
        assert!(has_sensitive_component_with_case(
            Path::new("C:\\Keys\\ID_RSA"),
            true
        ));
        assert!(!has_sensitive_component_with_case(
            Path::new("C:\\Keys\\ID_RSA"),
            false
        ));
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
            // The absolute trailing-slash and glob faces of a directory entry.
            "rm -rf /etc/ssh/",
            "rm /etc/ssh/*",
            "cat /var/log/auth.log",
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
    fn no_allowance_at_another_layer_can_un_deny_the_builtin_set() {
        // A user-layer trusted prefix AND a typed allow rule both name
        // `cat /etc/shadow`; the module doc's "deny always wins across
        // layers" says neither may un-deny the builtin ruleset.
        let user_allowance = Ruleset::user(vec!["cat /etc/shadow".to_string()], Vec::new())
            .with_ask_rules(vec![ToolAskRule {
                action: PermissionAction::Allow,
                ..ToolAskRule::exec_shell("cat /etc/shadow")
            }]);
        let engine =
            ExecPolicyEngine::with_rulesets(vec![builtin_safety_ruleset(), user_allowance]);
        let decision = engine.check(ctx("cat /etc/shadow")).unwrap();
        assert!(
            matches!(
                decision.requirement,
                ExecApprovalRequirement::Forbidden { .. }
            ),
            "user-layer allowance must not un-deny the builtin deny: {decision:?}"
        );
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
