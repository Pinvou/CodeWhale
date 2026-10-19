export const STARTUP_UPDATE_DELAY_MS = 15000;

/** One delayed attempt after painting. Cancellation never schedules a retry. */
export function scheduleStartupUpdateCheck({ available, requestFrame, cancelFrame, setTimer, clearTimer, check, onReport }) {
  if (!available) return () => {};
  let alive = true;
  let secondFrame;
  let timer;
  const firstFrame = requestFrame(() => {
    secondFrame = requestFrame(() => {
      timer = setTimer(() => {
        Promise.resolve().then(check).then((report) => {
          if (alive) onReport(report);
        }).catch(() => {});
      }, STARTUP_UPDATE_DELAY_MS);
    });
  });
  return () => {
    alive = false;
    cancelFrame(firstFrame);
    if (secondFrame !== undefined) cancelFrame(secondFrame);
    if (timer !== undefined) clearTimer(timer);
  };
}

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
