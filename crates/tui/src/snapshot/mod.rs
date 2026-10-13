//! Workspace snapshots — pre/post-turn safety net.
//!
//! Each turn the engine takes a `pre-turn:<seq>` snapshot of the user's
//! workspace into a side git repo at
//! `~/.deepseek/snapshots/<project_hash>/<worktree_hash>/.git`, then a
//! matching `post-turn:<seq>` snapshot when the turn finishes. Users
//! can roll back via `/restore N` (slash command) or, when the model
//! recognises an "undo my last edit" intent, the `revert_turn` tool.
//!
//! ## Why a side repo?
//!
//! - The user's own `.git` is never touched. `--git-dir` and
//!   `--work-tree` are *always* set together when we shell out to git;
//!   that single invariant is what keeps snapshots and the user's repo
//!   completely independent.
//! - Workspaces without git still get snapshots.
//! - `git`'s own deduplication (object packfiles) keeps the disk
//!   footprint tractable — typical 100 MB workspace × 12 turns ≈ 1.2 GB
//!   uncompressed but git's content-addressed storage usually brings
//!   that down 10-30×. We mitigate further with:
//!     - 7-day default retention (`session_manager` prunes at session
//!       start via [`prune::prune_older_than`]).
//!     - `gc.auto = 0` on the side repo (we don't want background gcs
//!       firing mid-turn) plus an explicit `git gc --prune=now` after
//!       prune.
//!     - Startup cleanup for stale `tmp_pack_*` files left by interrupted
//!       git pack operations.
//!
//! ## Failure model
//!
//! Pre/post-turn snapshot calls are **non-fatal**. If `git` is missing,
//! the disk is full, or the workspace is on a read-only filesystem, the
//! turn proceeds and the engine logs a warning. The snapshot is a
//! safety net, not a correctness gate.
//!
//! Workspaces over the configured size cap (`[snapshots] max_workspace_gb`,
//! default 2 GB of non-excluded content) skip snapshot init entirely. That
//! disable is intentionally loud: the operator is told once that undo is off
//! for the workspace, with the opt-in knobs (raise the cap, or set
//! `max_workspace_gb = 0` to disable the size gate). Scoped snapshot roots are
//! not yet a first-class config; the practical opt-in today is the cap override.

pub mod paths;
pub mod prune;
pub mod repo;

#[allow(unused_imports)]
pub use paths::{snapshot_dir_for, snapshot_git_dir};
pub use prune::{DEFAULT_MAX_AGE, prune_older_than};

/// Maximum snapshots kept per workspace side-repo. Oldest are pruned
/// after each new snapshot to cap disk usage (#1112).
pub const DEFAULT_MAX_SNAPSHOTS: usize = 50;

/// Honesty clause for revert/undo reports when the session root set extends
/// beyond the primary workspace: the snapshot side-repo is rooted at the
/// primary, so a restore rolls back only the primary while attached-root
/// writes persist. Appended verbatim so single-root reports stay
/// byte-identical (the clause is simply never added there).
pub const ATTACHED_ROOTS_NOT_REVERTED_NOTE: &str =
    "Only the primary workspace was reverted; attached workspace roots were not rolled back.";

/// Round-43 P2-5: the localized form for human-facing TUI surfaces (the
/// model/API lanes keep the stable English constant above).
pub fn attached_roots_not_reverted_note(locale: crate::localization::Locale) -> String {
    crate::localization::tr(
        locale,
        crate::localization::MessageId::AttachedRootsNotRevertedNote,
    )
    .into_owned()
}

/// Whether a restore from the workspace snapshot repo covers only the
/// primary root — true exactly when the normalized root set has an entry
/// OUTSIDE the primary tree. Revert/undo reports must carry
/// [`ATTACHED_ROOTS_NOT_REVERTED_NOTE`] in that case instead of implying a
/// full rollback.
///
/// A root nested under the primary does NOT count
/// (review #484/CodeWhale round-22 N22-3): `validate_workspace_roots`
/// explicitly allows attached roots under the primary, and the side repo's
/// work-tree IS the primary tree, so `repo.restore` reverts those writes
/// with the primary — claiming they "were not rolled back" would tell the
/// user the opposite of what happened.
///
/// Round-23 B23-1 + round-24 B24-1: BOTH spellings of every side are judged.
/// The lexically normalized forms catch `..`-spelled roots (`/ws/../shared`
/// collapses outside the primary); the canonical-or-raw forms (the
/// `ToolContext::boundary_roots` idiom) catch inward-symlink roots
/// (`/ws/link → /elsewhere`) that lexically nest under the primary while
/// every consumer canonicalizes them outside — writes through such a root
/// persist past the primary-rooted restore, so withholding the note for
/// either class was fail-unsafe (and an earlier doc asserted the opposite
/// of the code). The clause fires when EITHER form lands outside the
/// primary; over-disclosing the boundary is the safe side.
pub fn restore_covers_primary_only(
    workspace: &std::path::Path,
    workspace_roots: &[std::path::PathBuf],
) -> bool {
    let workspace_lexical = codewhale_core::normalize_path_lexically(workspace);
    let workspace_canonical = codewhale_core::normalize_path_lexically(
        &workspace
            .canonicalize()
            .unwrap_or_else(|_| workspace.to_path_buf()),
    );
    codewhale_core::normalize_workspace_roots(workspace, workspace_roots)
        .iter()
        .skip(1)
        .any(|root| {
            // Round-43 P2-2: coverage is a property of the SNAPSHOT, not of
            // directory containment — the side repo's built-in excludes
            // (vendor/, node_modules/, …) keep a nested attached root's
            // files OUT of the snapshot, and a nested repository records
            // only a mode-160000 gitlink, so neither reverts with the
            // primary even though both sit inside the primary tree. The
            // disclosure fires for those shapes too (fail-safe direction:
            // over-disclosing the boundary).
            if nested_root_escapes_snapshot_coverage(workspace, root) {
                return true;
            }
            let root_lexical = codewhale_core::normalize_path_lexically(root);
            let root_canonical = codewhale_core::normalize_path_lexically(
                &root.canonicalize().unwrap_or_else(|_| root.clone()),
            );
            !root_lexical.starts_with(&workspace_lexical)
                || !root_canonical.starts_with(&workspace_canonical)
        })
}

/// Round-43 P2-2: true when `root` nests under the primary lexically but
/// its CONTENT will not revert with a primary-rooted restore — the snapshot
/// repo's built-in excludes drop it, or it is (or sits under) a nested
/// repository the snapshot records only as a gitlink. Both shapes leave the
/// attached root's writes persisting past the restore, so the disclosure
/// must fire exactly as for an out-of-tree root.
fn nested_root_escapes_snapshot_coverage(
    workspace: &std::path::Path,
    root: &std::path::Path,
) -> bool {
    /// The snapshot repo's built-in exclude list (repo.rs BUILTIN_EXCLUDES):
    /// a nested attached root at or under one of these directories is not
    /// captured by the snapshot at all.
    const SNAPSHOT_EXCLUDED_DIRS: &[&str] = &[
        "node_modules",
        "target",
        "dist",
        "build",
        ".build",
        ".next",
        ".nuxt",
        ".svelte-kit",
        ".turbo",
        ".parcel-cache",
        "vendor",
        ".cargo",
        ".rustup",
        ".npm",
        ".bun",
        ".yarn",
        ".pnpm-store",
        ".cache",
        ".venv",
        "venv",
        ".tox",
        "__pycache__",
    ];

    let rel = root.strip_prefix(workspace).ok();
    let Some(rel) = rel else {
        return false;
    };
    // A nested repository boundary: the attached root (or any ancestor under
    // the primary up to it) carrying a .git entry records as a gitlink in
    // the primary's snapshot — its files never enter the snapshot tree.
    let mut probe = workspace.to_path_buf();
    for component in rel.components() {
        probe.push(component);
        if probe.join(".git").exists() {
            return true;
        }
    }
    // The excluded-directory shape: the root itself is one of the built-in
    // excludes (vendor/...) or sits under one (vendor/sub).
    let mut under_excluded = false;
    for component in rel.components() {
        let name = component.as_os_str().to_string_lossy();
        under_excluded = under_excluded || SNAPSHOT_EXCLUDED_DIRS.contains(&name.as_ref());
        if under_excluded {
            break;
        }
    }
    under_excluded
}
#[allow(unused_imports)]
pub use repo::{
    DEFAULT_MAX_WORKSPACE_BYTES_FOR_SNAPSHOT, Snapshot, SnapshotId, SnapshotRepo,
    estimate_workspace_size_bounded,
};

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    #[test]
    fn nested_under_primary_attached_roots_are_reverted_with_the_primary() {
        // Round-22 N22-3: the side repo's work-tree IS the primary tree, so a
        // root nested under it (validate_workspace_roots explicitly allows
        // those) is rolled back by the restore — the boundary note must not
        // claim otherwise.
        let workspace = PathBuf::from("/ws");
        assert!(restore_covers_primary_only(
            &workspace,
            &[PathBuf::from("/elsewhere")],
        ));
        assert!(!restore_covers_primary_only(
            &workspace,
            &[PathBuf::from("/ws/nested")],
        ));
        assert!(!restore_covers_primary_only(
            &workspace,
            &[PathBuf::from("/ws/nested/deeper")],
        ));
        // A root that contains the primary (`/`, a legacy-row ancestor) is
        // outside the reverted tree's spelling — its writes outside the
        // primary persist. A root equal to the primary dedups away in the
        // normalizer, like the empty set.
        assert!(restore_covers_primary_only(
            &workspace,
            &[PathBuf::from("/")],
        ));
        assert!(!restore_covers_primary_only(
            &workspace,
            &[PathBuf::from("/ws")],
        ));
        assert!(!restore_covers_primary_only(&workspace, &[]));
    }

    #[cfg(unix)]
    #[test]
    fn inward_symlink_root_fires_the_boundary_note() {
        // Round-24 B24-1: lexical normalization cannot see through an inward
        // symlink — `/ws/link → /elsewhere` lexically nests under `/ws`
        // while every consumer canonicalizes it outside, so the
        // lexical-only predicate withheld the boundary note while writes
        // through the root persisted past the primary-rooted restore. The
        // canonical leg (raw fallback for roots that do not resolve) must
        // fire it; a genuinely nested plain root must stay silent.
        let base = tempfile::tempdir().expect("tempdir");
        let workspace = base.path().join("ws");
        std::fs::create_dir_all(&workspace).expect("mkdir ws");
        let elsewhere = base.path().join("elsewhere");
        std::fs::create_dir_all(&elsewhere).expect("mkdir elsewhere");
        let link = workspace.join("link");
        std::os::unix::fs::symlink(&elsewhere, &link).expect("symlink");
        assert!(
            restore_covers_primary_only(&workspace, std::slice::from_ref(&link)),
            "an inward-symlink root must fire the boundary note"
        );
        // A plain root nested under the primary keeps the old behavior even
        // though the primary itself now also canonicalizes. A28-4: the dir
        // must EXIST first — a missing target canonicalizes to the raw
        // /var spelling while the primary canonicalizes to /private/var,
        // mixing spellings and spuriously firing the note on stock macOS.
        let nested_plain = workspace.join("nested");
        std::fs::create_dir_all(&nested_plain).expect("mkdir nested");
        assert!(!restore_covers_primary_only(&workspace, &[nested_plain]));
        // A symlink INSIDE the primary pointing at a nested directory stays
        // inside on both spellings — no note.
        let nested = workspace.join("nested-real");
        std::fs::create_dir_all(&nested).expect("mkdir nested");
        let inner_link = workspace.join("inner-link");
        std::os::unix::fs::symlink(&nested, &inner_link).expect("symlink");
        assert!(!restore_covers_primary_only(&workspace, &[inner_link]));

        // Round-43 P2-2: coverage follows the SNAPSHOT, not directory
        // containment — a vendor/ nested root (built-in exclude) and a
        // nested repository (gitlink-only) do not revert with the primary,
        // so the disclosure fires; an ordinary tracked nested root does
        // not (the plain-root case above).
        let vendor = workspace.join("vendor");
        std::fs::create_dir_all(&vendor).expect("mkdir vendor");
        assert!(
            restore_covers_primary_only(&workspace, std::slice::from_ref(&vendor)),
            "a vendor/ nested root is excluded from the snapshot — its writes persist"
        );
        let nested_repo = workspace.join("child-repo");
        std::fs::create_dir_all(nested_repo.join(".git")).expect("nested repo .git");
        assert!(
            restore_covers_primary_only(&workspace, std::slice::from_ref(&nested_repo)),
            "a nested repository records only a gitlink — its files never revert"
        );
        assert!(
            restore_covers_primary_only(&workspace, &[nested_repo.join("sub")]),
            "a root under a nested repository boundary escapes too"
        );
    }
}
