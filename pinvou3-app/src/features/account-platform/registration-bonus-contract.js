import { integerMoney, parseFinanceJSON, preserveJSONIntegerFields } from './finance-contract.js';

export const MAX_BONUS_POINTS = 922337203685477;
export function bonusRevision(value) {
  const revision = integerMoney(value);
  if (revision < 1n) throw new Error('INVALID_REGISTRATION_BONUS_ACK');
  return revision.toString();
}

// This endpoint alone has an int64 revision. Preserve its original token before
// JSON.parse; do not change how unrelated API versions are represented.
export function parseRegistrationBonusJSON(text) {
  const result = preserveJSONIntegerFields(text, key => ['revision', 'recoverable_registration_count', 'unrecoverable_registration_count'].includes(key), { rejectMalformed: false });
  const policy = parseFinanceJSON(result);
  policy.revision = bonusRevision(policy.revision);
  const amount = integerMoney(policy.amount_microcredits);
  if (amount < 0n || typeof policy.enabled !== 'boolean'
    || !Number.isInteger(policy.validity_days) || policy.validity_days < 1 || policy.validity_days > 3650
    || typeof policy.updated_at !== 'string' || !Number.isFinite(Date.parse(policy.updated_at))) throw new Error('INVALID_REGISTRATION_BONUS_ACK');
  for (const key of ['recoverable_registration_count', 'unrecoverable_registration_count']) {
    if (policy[key] !== undefined && integerMoney(policy[key]) < 0n) throw new Error('INVALID_REGISTRATION_BONUS_ACK');
  }
  return policy;
}

// Match the existing request contract: trim, then count Unicode code points.
// Native textarea lengths count UTF-16 units and cannot enforce this range.
export function registrationBonusReasonIsValid(value) {
  if (typeof value !== 'string') return false;
  const length = [...value.trim()].length;
  return length >= 10 && length <= 500;
}

export function registrationBonusFormIsValid(form) {
  return typeof form?.enabled === 'boolean'
    && typeof form.amount_points === 'string' && form.amount_points.trim() !== ''
    && Number.isSafeInteger(Number(form.amount_points)) && Number(form.amount_points) >= 0 && Number(form.amount_points) <= MAX_BONUS_POINTS
    && typeof form.validity_days === 'string' && form.validity_days.trim() !== ''
    && Number.isInteger(Number(form.validity_days)) && Number(form.validity_days) >= 1 && Number(form.validity_days) <= 3650
    && registrationBonusReasonIsValid(form.reason);
}

export function encodeRegistrationBonusJSON(value) {
  const revision = bonusRevision(value.expected_revision);
  if (typeof value.enabled !== 'boolean' || !Number.isSafeInteger(value.amount_points) || value.amount_points < 0 || value.amount_points > MAX_BONUS_POINTS
    || !Number.isInteger(value.validity_days) || value.validity_days < 1 || value.validity_days > 3650
    || !registrationBonusReasonIsValid(value.reason)) throw new Error('INVALID_REGISTRATION_BONUS_REQUEST');
  return `{"enabled":${value.enabled},"amount_points":${value.amount_points},"validity_days":${value.validity_days},"expected_revision":${revision},"reason":${JSON.stringify(value.reason.trim())}}`;
}
