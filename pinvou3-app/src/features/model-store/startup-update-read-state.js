// Keep acknowledgement separate from the updates returned by each startup check.
// The WebView's persistent storage survives normal application restarts.
import { VIEWED_UPDATES_STORAGE_KEY, readViewedUpdateSnapshot } from './startup-update-snapshot.js';
export { VIEWED_UPDATES_STORAGE_KEY };

function validKeys(value) {
  if (!Array.isArray(value)) return [];
  return value.flatMap((key) => {
    if (typeof key !== 'string') return [];
    try {
      const pair = JSON.parse(key);
      return Array.isArray(pair) && pair.length === 2 && pair.every((part) => typeof part === 'string' && part.length > 0)
        ? [JSON.stringify(pair)] : [];
    } catch {
      return [];
    }
  });
}

/** Parse the value captured synchronously when this app consumer mounted. */
export function parseViewedUpdateSnapshot(snapshot) {
  try {
    return new Set(validKeys(JSON.parse(snapshot)));
  } catch {
    return new Set();
  }
}

/** Missing, inaccessible or malformed storage must not break the model store. */
export function loadViewedUpdateRevisions(storage) {
  return parseViewedUpdateSnapshot(readViewedUpdateSnapshot(storage));
}

/** Merge before writing so another window's acknowledgements are retained. */
export function saveViewedUpdateRevisions(revisions, storage) {
  try {
    const target = storage === undefined ? globalThis.localStorage : storage;
    if (!target) return false;
    const saved = loadViewedUpdateRevisions(target);
    const merged = new Set([...saved, ...validKeys([...revisions])]);
    if (merged.size !== saved.size) target.setItem(VIEWED_UPDATES_STORAGE_KEY, JSON.stringify([...merged]));
    return true;
  } catch {
    // Keep in-memory acknowledgement when disk storage is unavailable or full.
    return false;
  }
}
