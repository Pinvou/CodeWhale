export const VIEWED_UPDATES_STORAGE_KEY = 'pinvou.model-store.viewed-updates.v1';

/** Capture the startup value now; parsing can wait until after painting. */
export function readViewedUpdateSnapshot(storage) {
  try {
    const target = storage === undefined ? globalThis.localStorage : storage;
    return target?.getItem(VIEWED_UPDATES_STORAGE_KEY) || '[]';
  } catch {
    return '[]';
  }
}
