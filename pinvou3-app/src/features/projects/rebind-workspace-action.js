// Loaded with the existing rebind dialog, after its user-triggered lazy boundary.
import { classifyRebindError, mergeRebindCarryoverIds } from './rebindErrors.js';

export async function runRebindWorkspace(confirmExisting, {
  bridge, rebindDraft, projectOpsBusy, setProjectOpsBusy, setRebindDraft, setSettingsToast, t, refreshCodexSessions,
}) {
        if (!bridge.projects || !rebindDraft || projectOpsBusy) return;
        setProjectOpsBusy(true);
        // Clear the previous attempt's inline error/busy hint so it does
        // not stack with this run's result.
        setRebindDraft(prev => prev && { ...prev, error: null, busySessionIds: null });
        // Feed the previous report's post-busy ids back (review #463
        // F-Major): a session an earlier run moved and reported post-busy is
        // routed by the backend into the same to-lane retry population as a
        // healthy session, so without the feed-back a busy-refused carryover
        // session would appear in NO report field and the dialog would close
        // claiming full success while its old-cwd runtime stays resident.
        // The backend honors only the intersection with its own retry
        // population, so this list can never widen the eviction set.
        const previousPostBusySessionIds = (rebindDraft.partial && rebindDraft.partial.postBusyIds) || [];
        try {
          const report = await bridge.projects.rebindWorkspaceRoot(
            rebindDraft.from, rebindDraft.to, confirmExisting, previousPostBusySessionIds);
          const rebound = (report && report.rebound_session_ids) ? report.rebound_session_ids.length : 0;
          const failed = (report && report.failed_session_ids) ? report.failed_session_ids.length : 0;
          const postBusy = (report && report.post_busy_session_ids) ? report.post_busy_session_ids.length : 0;
          // The dialog stays open whenever the report still has something the
          // user must act on — failed sessions to retry, or sessions whose
          // runtime the idle gate refused (round-8 MAJOR-2: closing on the
          // post-busy-only case left "retry once when idle" with no entry
          // point, because the unavailable-root badge disappears once the root
          // has moved — the badge renders strictly off the `available` wire
          // field, the backend's per-root is_dir() stat; review #463 round-14
          // B1 restored it after main #566 dropped it and badged every root).
          // Rerunning the backend with the same from/to converges
          // (the snapshot includes unsynced sessions; already-rebound ones are
          // no-ops). The post-busy ids are kept for the next retry's
          // feed-back (F-Major).
          if (failed > 0 || postBusy > 0) {
            setRebindDraft(prev => prev && {
              ...prev,
              // The run proceeded past the fence: the old-root warning is
              // consumed (review #463 round-14 should-fix 3) — a later
              // reappearance of the old root must re-arm the strong confirm.
              warnExisting: false,
              partial: {
                rebound,
                failed,
                failedIds: (report && report.failed_session_ids) || [],
                postBusy,
                postBusyIds: (report && report.post_busy_session_ids) || [],
              },
            });
          } else {
            setRebindDraft(null);
            if (rebound > 0) {
              setSettingsToast(t.uiProjects.rebindSuccess(rebound));
            } else {
              // A retry after everything already converged (or a root with
              // no sessions at all) returns an empty report; "Rebound 0"
              // would read as a failure (review #463 minor).
              setSettingsToast(t.uiProjects.rebindUpToDate);
            }
          }
          await refreshCodexSessions().catch((error) => {
            // Failure is not swallowed: the session list self-heals via the
            // session:list_changed event, but a silent gap after an explicit
            // failure must stay visible for troubleshooting
            // (review #463 minor).
            console.warn('refresh sessions after rebind failed', error);
          });
        } catch (error) {
          // Typed-marker matching (finding 11 / Minor 7 / round-8 M4): the
          // backend prefixes every user-reachable outcome with a stable ASCII
          // marker and we match only that prefix, never human copy. The mapping
          // lives in a pure helper so both halves of the contract are unit
          // tested (review #463 round-8 minor 10).
          const classified = classifyRebindError(error, t);
          // Carryover honesty (review #463 round-14 should-fix 1): a
          // roots-commit failure carries this run's moved ids as an error
          // suffix — the session lanes were already durable at `to`. Feed
          // them into the retry's post-busy carryover, or a refused eviction
          // of an already-rebound session is dropped (untouched this run,
          // carryover empty) and the dialog closes "up to date" with an
          // old-cwd runtime still resident.
          // The suffix ids UNION with the previous carryover (review #463
          // round-15 Major 1): replacing would drop a still-busy carryover
          // session from a prior run — it can never re-enter
          // rebound_session_ids (a converged to-lane session routes to
          // retry_evict_candidates only), so the next retry would close the
          // dialog "up to date" while its old-cwd runtime stays resident.
          // The backend honors only the intersection with its own retry
          // population, so the union cannot widen the eviction set.
          const carryoverPartial = (classified.reboundIds && classified.reboundIds.length > 0)
            ? (prev) => {
                // Full partial shape (the dialog reads failedIds.length
                // unconditionally): the moved sessions are conservatively
                // reported as post-busy — their runtime reclaim never ran on
                // the error path, so "retry when idle" is the honest state.
                const mergedIds = mergeRebindCarryoverIds(
                  prev.partial && prev.partial.postBusyIds,
                  classified.reboundIds,
                );
                // Spread first, counts after (review #463 round-17 SF-4): a
                // trailing spread dragged the PREVIOUS run's rebound/failed
                // counts into this error state — a mixed-run report where
                // the same id could appear in two contradicting rows. This
                // arm's only honest counts are the merged carryover. The
                // spread tolerates a null partial natively (object spread
                // ignores null/undefined; round-18 Blocker 1 — the explicit
                // `|| {}` fallback trips unicorn/no-useless-fallback-in-spread).
                return {
                  ...prev.partial,
                  rebound: 0,
                  failed: 0,
                  failedIds: [],
                  postBusy: mergedIds.length,
                  postBusyIds: mergedIds,
                  // Round-18 minor 1: the post-busy recheck never ran on an
                  // error path, so the "started a new turn during the rebind"
                  // diagnosis would be fabricated — the dialog renders the
                  // neutral carryover copy for this shape.
                  carryover: true,
                };
              }
            : null;
          if (classified.kind === 'old-root-exists') {
            setRebindDraft(prev => prev && { ...prev, warnExisting: true, error: null });
          } else if (classified.kind === 'sessions-busy') {
            // Busy rejection is the fence's high-frequency happy path
            // (Minor 7): map it to i18n copy; only session ids follow the
            // marker, and they are shown verbatim for troubleshooting.
            // warnExisting is kept as-is out of deliberate conservatism
            // (round-20 minor 14): both busy emission sites actually fire
            // AFTER the old-root check, so the flag is already consumed —
            // keeping it costs ZERO extra confirm round-trips (the comment
            // previously claimed one; inverted per round-24 minor 18).
            setRebindDraft(prev => prev && {
              ...prev,
              busySessionIds: classified.busySessionIds,
              error: null,
              ...(carryoverPartial ? { partial: carryoverPartial(prev) } : {}),
            });
          } else if (classified.kind === 'copy') {
            // Consume the old-root warning ONLY when the run actually
            // proceeded past the old-root check (round-17 SF-4): IN_PROGRESS
            // / TO_ROOT / TO_NESTED / TO_UNUSABLE all classify as `copy` and
            // all fire BEFORE require_confirm_existing, and clearing on them
            // would let a re-appeared old root ride the stale flag silently.
            // Clearing on a genuine post-fence failure (review #463 round-14
            // should-fix 3) still re-arms the strong confirm on a later
            // reappearance of the old root.
            setRebindDraft(prev => prev && {
              ...prev,
              warnExisting: classified.passedFence ? false : prev.warnExisting,
              error: classified.message,
              ...(carryoverPartial ? { partial: carryoverPartial(prev) } : {}),
            });
          } else {
            // Log hygiene (review #463 round-14 should-fix 5): the raw error
            // chain embeds host paths; it is shown in the dialog verbatim by
            // design, but stays out of the console. The classified kind is
            // the path-free diagnostic.
            console.warn('rebind workspace failed:', classified.kind);
            // On failure keep the dialog open with the error inline
            // (review #463 M7): in-place display persists and sits next to
            // the retry; an unmapped backend error is shown verbatim as a
            // diagnostic detail rather than guessed at. Its provenance is
            // unknown (round-17 SF-4), so the strong-confirm flag is kept:
            // an extra confirm round-trip at worst, never a skipped one.
            setRebindDraft(prev => prev && {
              ...prev,
              warnExisting: prev.warnExisting,
              error: classified.message,
              ...(carryoverPartial ? { partial: carryoverPartial(prev) } : {}),
            });
          }
        } finally {
          setProjectOpsBusy(false);
        }
}
