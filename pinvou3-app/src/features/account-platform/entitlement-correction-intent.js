import { requireFinanceID } from './finance-contract.js';
import { entitlementCorrectionApprovalDraft } from './admin-entitlement-correction.js';

const scopeName = 'entitlement-correction';
const prefix = 'pinvou.entitlement-correction.intent.v1:';
const storageKey = actor => prefix + requireFinanceID(actor);

function validate(value, actor, scope) {
  if (scope !== scopeName || value?.actor !== actor || value.scope !== scope ||
      !/^[!-~]{16,111}$/.test(value.key || '') || !value.facts ||
      typeof value.facts.reason !== 'string' || typeof value.facts.reasonCode !== 'string' ||
      Object.keys(value).sort().join(',') !== (value.editReference ? 'actor,editReference,facts,key,scope' : 'actor,facts,key,scope') ||
      Object.keys(value.facts).sort().join(',') !== 'precheck,reason,reasonCode') throw new Error('INVALID_ENTITLEMENT_INTENT');
  const draft = entitlementCorrectionApprovalDraft(value.facts.precheck, value.facts.reasonCode, value.facts.reason, value.key);
  if (value.editReference) {
    const { existing, input } = value.editReference;
    requireFinanceID(existing?.approval_request_id);
    if (typeof input !== 'string' || existing.command_type !== draft.command_type || existing.target_resource_id !== draft.target_resource_id ||
        existing.target_resource_type !== draft.target_resource_type || !Number.isSafeInteger(existing.version) || existing.version < 1 || existing.status !== 'DRAFT') throw new Error('INVALID_ENTITLEMENT_INTENT');
    const canonical = item => Array.isArray(item) ? item.map(canonical) : item && typeof item === 'object' ? Object.fromEntries(Object.keys(item).sort().map(key => [key, canonical(item[key])])) : item;
    if (input !== JSON.stringify(canonical(draft))) throw new Error('INVALID_ENTITLEMENT_INTENT');
  }
  return value;
}

export function readEntitlementIntent(actor, scope, storage = globalThis.sessionStorage) {
  const raw = storage.getItem(storageKey(actor));
  if (raw === null) return null;
  if (raw.length > 64 * 1024) throw new Error('INVALID_ENTITLEMENT_INTENT');
  return validate(JSON.parse(raw), actor, scope);
}

export function saveEntitlementIntent(value, storage = globalThis.sessionStorage) {
  validate(value, value.actor, value.scope);
  const old = readEntitlementIntent(value.actor, value.scope, storage);
  const base = item => ({ actor: item.actor, scope: item.scope, key: item.key, facts: item.facts });
  if (old && JSON.stringify(base(old)) !== JSON.stringify(base(value))) throw new Error('ENTITLEMENT_RECOVERY_UNAVAILABLE');
  const raw = JSON.stringify(old || value);
  storage.setItem(storageKey(value.actor), raw);
  if (storage.getItem(storageKey(value.actor)) !== raw) throw new Error('ENTITLEMENT_RECOVERY_UNAVAILABLE');
  return readEntitlementIntent(value.actor, value.scope, storage);
}

export function clearEntitlementIntent(actor, scope, key, storage = globalThis.sessionStorage) {
  const current = readEntitlementIntent(actor, scope, storage);
  if (current && current.key !== key) return;
  storage.removeItem(storageKey(actor));
  if (storage.getItem(storageKey(actor)) !== null) throw new Error('ENTITLEMENT_RECOVERY_UNAVAILABLE');
}

export const entitlementIntentStorage = Object.freeze({ read: readEntitlementIntent, save: saveEntitlementIntent, clear: clearEntitlementIntent });

// The edit CAS belongs to the frozen invocation, not the latest client or its
// epoch. Persist before the first PUT; keep it until the operation clears its
// whole intent after an authoritative receipt, including clear failures.
export function entitlementEditReferences(actor, key, storage = globalThis.sessionStorage) {
  const current = () => {
    const value = readEntitlementIntent(actor, scopeName, storage);
    if (!value || value.key !== key) throw new Error('ENTITLEMENT_RECOVERY_UNAVAILABLE');
    return value;
  };
  return {
    get: () => current().editReference,
    set: (_identity, reference) => {
      const value = current();
      const existing = reference.existing;
      const next = { ...value, editReference: { input: reference.input, existing: {
        approval_request_id: existing.approval_request_id, command_type: existing.command_type,
        target_resource_type: existing.target?.resource_type || existing.target_resource_type,
        target_resource_id: existing.target?.resource_id || existing.target_resource_id,
        version: existing.version, status: existing.status,
      } } };
      validate(next, actor, scopeName);
      if (value.editReference && JSON.stringify(value.editReference) !== JSON.stringify(next.editReference)) throw new Error('ENTITLEMENT_RECOVERY_UNAVAILABLE');
      const raw = JSON.stringify(next); storage.setItem(storageKey(actor), raw);
      if (storage.getItem(storageKey(actor)) !== raw) throw new Error('ENTITLEMENT_RECOVERY_UNAVAILABLE');
      current();
    },
    delete: () => {},
  };
}
