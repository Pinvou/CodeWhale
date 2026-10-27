import { invokeTauri } from '../../platform/tauri/client.js';
import { warnStartupUpdateFailure } from './startup-update-scheduler.js';
import { parseViewedUpdateSnapshot, saveViewedUpdateRevisions } from './startup-update-read-state.js';

/** Keep only results that still match the installed package checked at startup. */
export function updatesForLibrary(updates, models) {
  if (!Array.isArray(models)) return updates;
  const current = new Map(models.map((model) => [model.card_id, model.card_revision]));
  const next = updates.filter((update) => current.get(update.card_id) === update.local_revision);
  return next.length === updates.length ? updates : next;
}

const updateKey = (update) => JSON.stringify([update.card_id, update.revision]);

export function initialStartupUpdateState(viewedRevisions = []) {
  return { updates: [], viewedRevisions: new Set(viewedRevisions) };
}

/** Sidebar acknowledgement and available model versions have separate lifetimes. */
export function reduceStartupUpdates(state, action) {
  if (action.type === 'library') {
    const updates = updatesForLibrary(state.updates, action.models);
    return updates === state.updates ? state : { ...state, updates };
  }
  if (action.type === 'report') {
    const next = { ...state, updates: updatesForLibrary(action.updates, action.models) };
    return action.viewingStore ? reduceStartupUpdates(next, { type: 'viewed' }) : next;
  }
  if (action.type === 'viewed') {
    if (!hasUnreadStartupUpdates(state)) return state;
    return { ...state, viewedRevisions: new Set([...state.viewedRevisions, ...state.updates.map(updateKey)]) };
  }
  return state;
}

export function hasUnreadStartupUpdates(state) {
  return state.updates.some((update) => !state.viewedRevisions.has(updateKey(update)));
}

/** Own the reminder state once its implementation has loaded. */
export function createStartupUpdateController(snapshot, emit, storage) {
  let state = initialStartupUpdateState(parseViewedUpdateSnapshot(snapshot));
  const publish = () => emit({ updates: state.updates, hasUnread: hasUnreadStartupUpdates(state) });
  saveViewedUpdateRevisions(state.viewedRevisions, storage);
  publish();
  return {
    send(action) {
      const next = reduceStartupUpdates(state, action);
      if (next !== state) {
        if (next.viewedRevisions !== state.viewedRevisions) {
          saveViewedUpdateRevisions(next.viewedRevisions, storage);
        }
        state = next;
        publish();
      }
    },
  };
}

/** The timer has a lower bound; slow runtime loading never starts it early. */
export function scheduleDelayedStartupUpdateCheck({ deadline, host = globalThis, check, onReport }) {
  let alive = true;
  const timer = host.setTimeout(() => {
    Promise.resolve().then(check).then((report) => {
      if (alive) onReport(report);
    }).catch(warnStartupUpdateFailure);
  }, Math.max(0, deadline - host.performance.now()));
  return () => { alive = false; host.clearTimeout(timer); };
}

export function startStartupUpdateCheck(context, deadline) {
  return scheduleDelayedStartupUpdateCheck({
    deadline,
    check: () => invokeTauri('model_store_startup_update_check'),
    onReport: (report) => context.controller.send({
      type: 'report', updates: Array.isArray(report?.updates) ? report.updates : [],
      models: context.models, viewingStore: context.viewingStore,
    }),
  });
}
