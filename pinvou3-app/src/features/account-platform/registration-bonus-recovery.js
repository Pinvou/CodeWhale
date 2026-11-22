import { encodeRegistrationBonusJSON } from './registration-bonus-contract.js';
const storageKey = actor => `pinvou.admin.registration-bonus.pending.${actor}`;

export function bonusAttemptProvesNoWrite(error, originallyUnknown = false) {
  if (originallyUnknown) return false;
  const statuses = {
    REGISTRATION_BONUS_INVALID: [400], REGISTRATION_BONUS_CONFLICT: [409],
    REGISTRATION_BONUS_ELIGIBILITY_NOT_READY: [409], RECENT_AUTHENTICATION_REQUIRED: [401, 403],
    AUTH_REQUIRED: [401], PERMISSION_DENIED: [403], CSRF_REJECTED: [403],
  };
  return statuses[error?.code]?.includes(error?.status) === true;
}

export function readBonusPending(actor, storage = globalThis.sessionStorage) {
  const raw = storage.getItem(storageKey(actor));
  if (!raw) return null;
  const pending = JSON.parse(raw);
  if (pending.actor !== actor || !/^registration-bonus-[0-9a-f-]{36}$/.test(pending.key)) throw new Error('INVALID_REGISTRATION_BONUS_RECOVERY');
  encodeRegistrationBonusJSON(pending.body);
  return pending;
}
export function persistBonusPending(pending, storage = globalThis.sessionStorage) {
  encodeRegistrationBonusJSON(pending.body);
  const raw = JSON.stringify(pending);
  storage.setItem(storageKey(pending.actor), raw);
  if (storage.getItem(storageKey(pending.actor)) !== raw) throw new Error('INVALID_REGISTRATION_BONUS_RECOVERY');
}
export function clearBonusPending(actor, storage = globalThis.sessionStorage) {
  storage.removeItem(storageKey(actor));
  if (storage.getItem(storageKey(actor)) !== null) throw new Error('INVALID_REGISTRATION_BONUS_RECOVERY');
}
