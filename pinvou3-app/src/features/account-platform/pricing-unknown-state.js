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
    records.set(key, { value: kinds.has(value?.kind) ? value : null, listeners: new Set() });
  }
  return records.get(key);
}

export function pricingUnknownSnapshot(key) { return record(key).value; }

export function subscribePricingUnknown(key, listener) {
  const state = record(key);
  state.listeners.add(listener);
  return () => state.listeners.delete(listener);
}

export function storePricingUnknown(key, value, expected) {
  const state = record(key);
  if (expected !== undefined && state.value !== expected) return false;
  state.value = value?.phase === 'PREPARING' ? null : value;
  try {
    if (value) globalThis.sessionStorage?.setItem(key, JSON.stringify(value));
    else globalThis.sessionStorage?.removeItem(key);
  } catch { /* The shared live record remains authoritative in this page. */ }
  for (const listener of state.listeners) listener();
  return true;
}
