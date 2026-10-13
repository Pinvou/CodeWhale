import { invokeTauri, isTauriAvailable } from './client.js';

/** @param {string} message @param {unknown} error */
const defaultWarn = (message, error) => console.warn(message, error);

/**
 * Commit an installed update only after the main React tree reaches its first
 * layout commit. Browser preview takes the zero-IPC unavailable path.
 */
export async function commitPendingUpdateReady({
  available = isTauriAvailable,
  invoke = invokeTauri,
  warn = defaultWarn,
} = {}) {
  if (!available()) return null;
  try {
    return await invoke('commit_pending_update_ready');
  } catch (error) {
    warn('[startup] failed to commit pending update readiness', error);
    return null;
  }
}
