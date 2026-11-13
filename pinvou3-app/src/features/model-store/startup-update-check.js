import { prepareAfterStartupPaint } from './startup-update-scheduler.js';
import { scheduleDelayedStartupUpdateCheck } from './startup-update-runtime.js';

export { STARTUP_UPDATE_DELAY_MS } from './startup-update-scheduler.js';
export { updatesForLibrary, initialStartupUpdateState, reduceStartupUpdates, hasUnreadStartupUpdates } from './startup-update-runtime.js';

/** Compatibility entry point for a caller that already has its check logic. */
export function scheduleStartupUpdateCheck({ available, host = globalThis, check, onReport, prepare }) {
  if (!available) return () => {};
  let cleanup;
  const cancelPaint = prepareAfterStartupPaint((deadline) => {
    prepare?.();
    cleanup = scheduleDelayedStartupUpdateCheck({ deadline, host, check, onReport });
  }, host);
  return () => { cancelPaint(); cleanup?.(); };
}
