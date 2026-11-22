// Every mounted pricing presenter for one actor/session observes one accepted
// operation. Optional browser storage extends recovery across a page reload;
// its failure must not split the live owners into independent writable states.
const records = new Map();
const kinds = new Set(['DRAFT_CREATE', 'DRAFT_UPDATE', 'UNIT_CREATE', 'UNIT_DEACTIVATE', 'PRICE_PUBLISH', 'PRICE_TRANSITION']);

function record(key) {
  if (!records.has(key)) {
    let value = null;
    try {
      value = JSON.parse(globalThis.sessionStorage?.getItem(key) || 'null');
      if (value?.phase === 'PREPARING') { globalThis.sessionStorage?.removeItem(key); value = null; }
    } catch { /* Browser storage is optional. */ }
    records.set(key, { snapshot: { value: kinds.has(value?.kind) ? value : null, revision: 0 }, listeners: new Set() });
  }
  return records.get(key);
}

export function pricingUnknownStateSnapshot(key) { return record(key).snapshot; }
export function pricingUnknownSnapshot(key) { return pricingUnknownStateSnapshot(key).value; }

export function subscribePricingUnknown(key, listener) {
  const state = record(key);
  state.listeners.add(listener);
  return () => state.listeners.delete(listener);
}

export function storePricingUnknown(key, value, expected, { retireIntents = false } = {}) {
  const state = record(key);
  if (expected !== undefined && state.snapshot.value !== expected) return false;
  const next = value?.phase === 'PREPARING' ? null : value;
  // Only explicit UNKNOWN completion/archival retires forms. Ordinary pending
  // writes, successes and initial validation failures preserve existing UX.
  const revision = state.snapshot.revision + (retireIntents && state.snapshot.value && next == null ? 1 : 0);
  state.snapshot = { value: next, revision };
  try {
    if (value) globalThis.sessionStorage?.setItem(key, JSON.stringify(value));
    else globalThis.sessionStorage?.removeItem(key);
  } catch { /* The shared live record remains authoritative in this page. */ }
  for (const listener of state.listeners) listener();
  return true;
}
