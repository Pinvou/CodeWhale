// Keep acknowledgement separate from the updates returned by each startup check.
// The WebView's persistent storage survives normal application restarts.
export const VIEWED_UPDATES_STORAGE_KEY = 'pinvou.model-store.viewed-updates.v1';

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

/** Missing, inaccessible or malformed storage must not break the model store. */
export function loadViewedUpdateRevisions(storage) {
  try {
    const target = storage === undefined ? globalThis.localStorage : storage;
    return new Set(validKeys(JSON.parse(target?.getItem(VIEWED_UPDATES_STORAGE_KEY) || '[]')));
  } catch {
    return new Set();
  }
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
