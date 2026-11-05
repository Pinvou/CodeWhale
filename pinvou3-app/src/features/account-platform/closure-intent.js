import { requireFinanceID } from './finance-contract.js';

const prefix = 'pinvou.closure.intent.v1:';
const keyFor = (actor, scope) => `${prefix}${actor}:${scope}`;
const reasonValid = value => typeof value === 'string' && [...value].length >= 1 && [...value].length <= 500;

export function validateClosureFacts(scope, facts) {
  if (!facts || typeof facts !== 'object' || Array.isArray(facts)) throw new Error('INVALID_CLOSURE_INTENT');
  const keys = Object.keys(facts).sort().join(',');
  if (scope === 'account-closure') {
    if (keys !== 'confirmation' || facts.confirmation !== 'CLOSE_MY_ACCOUNT') throw new Error('INVALID_CLOSURE_INTENT');
  } else if (scope.startsWith('account-closure-retry:')) {
    requireFinanceID(scope.slice('account-closure-retry:'.length));
    if (keys !== 'id,reason' || facts.id !== scope.slice('account-closure-retry:'.length) || !reasonValid(facts.reason)) throw new Error('INVALID_CLOSURE_INTENT');
  } else if (scope.startsWith('closed-grant-reclaim:')) {
    const parts = scope.slice('closed-grant-reclaim:'.length).split(':');
    if (parts.length !== 2) throw new Error('INVALID_CLOSURE_INTENT');
    const [org, grant] = parts;
    requireFinanceID(org); requireFinanceID(grant);
    if (keys !== 'grant,reason' || facts.grant !== grant || !reasonValid(facts.reason)) throw new Error('INVALID_CLOSURE_INTENT');
  } else if (scope.startsWith('closed-grant-reclaim-batch:')) {
    requireFinanceID(scope.slice('closed-grant-reclaim-batch:'.length));
    if (keys !== 'grants,reason' || !reasonValid(facts.reason) || !Array.isArray(facts.grants) || facts.grants.length < 1 || facts.grants.length > 200 || new Set(facts.grants).size !== facts.grants.length) throw new Error('INVALID_CLOSURE_INTENT');
    facts.grants.forEach(requireFinanceID);
  } else throw new Error('INVALID_CLOSURE_INTENT');
  return facts;
}

export function readClosureIntent(actor, scope, storage = globalThis.sessionStorage) {
  requireFinanceID(actor);
  const text = storage.getItem(keyFor(actor, scope));
  if (text === null) return null;
  if (text.length > 64 * 1024) throw new Error('INVALID_CLOSURE_INTENT');
  const value = JSON.parse(text);
  if (value.actor !== actor || value.scope !== scope || typeof value.key !== 'string' || !/^[!-~]{16,128}$/.test(value.key)) throw new Error('INVALID_CLOSURE_INTENT');
  validateClosureFacts(scope, value.facts);
  return value;
}

export function saveClosureIntent(value, storage = globalThis.sessionStorage) {
  requireFinanceID(value.actor); validateClosureFacts(value.scope, value.facts);
  const previous = readClosureIntent(value.actor, value.scope, storage);
  if (previous && JSON.stringify(previous) !== JSON.stringify(value)) throw new Error('CLOSURE_RECOVERY_UNAVAILABLE');
  const text = JSON.stringify(value);
  storage.setItem(keyFor(value.actor, value.scope), text);
  if (storage.getItem(keyFor(value.actor, value.scope)) !== text) throw new Error('CLOSURE_RECOVERY_UNAVAILABLE');
  return readClosureIntent(value.actor, value.scope, storage);
}

export function clearClosureIntent(actor, scope, expectedKey, storage = globalThis.sessionStorage) {
  const current = readClosureIntent(actor, scope, storage);
  if (current && current.key !== expectedKey) return;
  storage.removeItem(keyFor(actor, scope));
  if (storage.getItem(keyFor(actor, scope)) !== null) throw new Error('CLOSURE_RECOVERY_UNAVAILABLE');
}

export function closureRepairIntents(actor, storage = globalThis.sessionStorage) {
  return intentsWithPrefix(actor, 'account-closure-retry:', storage);
}

export function closureGrantIntents(actor, org, storage = globalThis.sessionStorage) {
  requireFinanceID(org);
  return intentsWithPrefix(actor, `closed-grant-reclaim:${org}:`, storage);
}

function intentsWithPrefix(actor, scopePrefix, storage) {
  requireFinanceID(actor);
  const start = `${prefix}${actor}:${scopePrefix}`;
  const values = [];
  for (let index = 0; index < storage.length; index++) {
    const key = storage.key(index);
    if (key?.startsWith(start)) values.push(readClosureIntent(actor, key.slice(`${prefix}${actor}:`.length), storage));
  }
  return values;
}
