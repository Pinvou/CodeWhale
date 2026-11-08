import { requireFinanceID } from './finance-contract.js';
import { financialRepairApprovalDraft } from './admin-financial-repair-drafts.js';

const scopeName = 'ledger-repair-draft';
const storageKey = actor => `pinvou.ledger-repair.intent.v1:${requireFinanceID(actor)}`;
function validate(value, actor, scope) {
  if (value?.actor !== actor || scope !== scopeName || value.scope !== scope ||
      Object.keys(value).sort().join(',') !== 'actor,facts,key,scope' ||
      Object.keys(value.facts || {}).sort().join(',') !== 'projection,reason,requestKey' ||
      typeof value.key !== 'string' || !value.key || value.key !== value.key.trim() || new TextEncoder().encode(value.key).length > 111 ||
      value.key !== value.facts.requestKey) throw new Error('INVALID_LEDGER_INTENT');
  financialRepairApprovalDraft(value.facts.projection, value.facts.reason, value.key);
  return value;
}
export function readLedgerIntent(actor, scope, storage = globalThis.sessionStorage) {
  const raw = storage.getItem(storageKey(actor));
  if (raw === null) return null;
  if (raw.length > 64 * 1024) throw new Error('INVALID_LEDGER_INTENT');
  return validate(JSON.parse(raw), actor, scope);
}
export function saveLedgerIntent(value, storage = globalThis.sessionStorage) {
  const next = value;
  validate(next, next.actor, next.scope);
  const old = readLedgerIntent(next.actor, next.scope, storage);
  if (old && JSON.stringify(old) !== JSON.stringify(next)) throw new Error('LEDGER_RECOVERY_UNAVAILABLE');
  const raw = JSON.stringify(old || next);
  storage.setItem(storageKey(next.actor), raw);
  if (storage.getItem(storageKey(next.actor)) !== raw) throw new Error('LEDGER_RECOVERY_UNAVAILABLE');
  return readLedgerIntent(next.actor, next.scope, storage);
}
export function clearLedgerIntent(actor, scope, key, storage = globalThis.sessionStorage) {
  const current = readLedgerIntent(actor, scope, storage);
  if (current && current.key !== key) return;
  storage.removeItem(storageKey(actor));
  if (storage.getItem(storageKey(actor)) !== null) throw new Error('LEDGER_RECOVERY_UNAVAILABLE');
}
export const ledgerIntentStorage = Object.freeze({
  read: readLedgerIntent, save: saveLedgerIntent, clear: clearLedgerIntent,
  clearRecord: (record, storage = globalThis.sessionStorage) => {
    validate(record, record.actor, record.scope);
    const current = readLedgerIntent(record.actor, record.scope, storage);
    if (!current || JSON.stringify(current) !== JSON.stringify(record)) throw new Error('LEDGER_RECOVERY_UNAVAILABLE');
    storage.removeItem(storageKey(record.actor));
    if (storage.getItem(storageKey(record.actor)) !== null) throw new Error('LEDGER_RECOVERY_UNAVAILABLE');
  },
  createIntent: (actor, scope, facts) => validate({ actor, scope, facts, key: facts.requestKey }, actor, scope),
});
