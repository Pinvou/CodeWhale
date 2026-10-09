import { integerMoney, parseFinanceJSON } from './finance-contract.js';

export const MAX_BONUS_POINTS = 922337203685477;
export function bonusRevision(value) {
  const revision = integerMoney(value);
  if (revision < 1n) throw new Error('INVALID_REGISTRATION_BONUS_ACK');
  return revision.toString();
}

// This endpoint alone has an int64 revision. Preserve its original token before
// JSON.parse; do not change how unrelated API versions are represented.
export function parseRegistrationBonusJSON(text) {
  let result = '';
  for (let i = 0; i < text.length;) {
    if (text[i] !== '"') { result += text[i++]; continue; }
    const start = i++;
    while (i < text.length) {
      if (text[i] === '\\') { i += 2; continue; }
      if (text[i++] === '"') break;
    }
    const token = text.slice(start, i);
    result += token;
    let cursor = i;
    while (/\s/.test(text[cursor] || '') && cursor < text.length) cursor++;
    if (text[cursor] !== ':' || !['revision', 'recoverable_registration_count', 'unrecoverable_registration_count'].includes(JSON.parse(token))) continue;
    cursor++;
    while (/\s/.test(text[cursor] || '') && cursor < text.length) cursor++;
    const match = text.slice(cursor).match(/^-?(?:0|[1-9]\d*)(?=\s*[,}\]])/);
    if (!match) continue;
    integerMoney(match[0]);
    result += text.slice(i, cursor) + JSON.stringify(match[0]);
    i = cursor + match[0].length;
  }
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

export function encodeRegistrationBonusJSON(value) {
  const revision = bonusRevision(value.expected_revision);
  if (typeof value.enabled !== 'boolean' || !Number.isSafeInteger(value.amount_points) || value.amount_points < 0 || value.amount_points > MAX_BONUS_POINTS
    || !Number.isInteger(value.validity_days) || value.validity_days < 1 || value.validity_days > 3650
    || typeof value.reason !== 'string' || [...value.reason.trim()].length < 10 || [...value.reason.trim()].length > 500) throw new Error('INVALID_REGISTRATION_BONUS_REQUEST');
  return `{"enabled":${value.enabled},"amount_points":${value.amount_points},"validity_days":${value.validity_days},"expected_revision":${revision},"reason":${JSON.stringify(value.reason.trim())}}`;
}
