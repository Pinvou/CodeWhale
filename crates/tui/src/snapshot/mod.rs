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
/// Round-23 B23-1: BOTH sides are lexically normalized before the
/// containment comparison. Over the raw persisted spellings a `..`-spelled
/// root (`/ws/../shared`) or an inward-symlink root (`/ws/link →
/// /elsewhere`) component-wise "nests" under the primary while every
/// consumer canonicalizes it outside — writes there persist through the
/// primary-rooted restore, so withholding the note for those spellings was
/// fail-unsafe (and the previous doc asserted the opposite of the code).
/// Normalizing first fires the note for exactly the roots whose consumers
/// see them outside the primary; over-disclosing the boundary is the safe
/// side.
pub fn restore_covers_primary_only(
    workspace: &std::path::Path,
    workspace_roots: &[std::path::PathBuf],
) -> bool {
    let workspace_lexical = codewhale_core::normalize_path_lexically(workspace);
    codewhale_core::normalize_workspace_roots(workspace, workspace_roots)
        .iter()
        .skip(1)
        .any(|root| {
            let root_lexical = codewhale_core::normalize_path_lexically(root);
            !root_lexical.starts_with(&workspace_lexical)
        })
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
}
