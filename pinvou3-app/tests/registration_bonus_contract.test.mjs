import test from 'node:test';
import assert from 'node:assert/strict';
import { parseRegistrationBonusJSON, encodeRegistrationBonusJSON, bonusRevision, MAX_BONUS_POINTS } from '../src/features/account-platform/registration-bonus-contract.js';
import { readBonusPending, persistBonusPending, clearBonusPending, bonusAttemptProvesNoWrite } from '../src/features/account-platform/registration-bonus-recovery.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { adminPlatformCopy } from '../src/shared/i18n.js';
const policy = revision => `{"revision":${revision},"enabled":true,"amount_microcredits":9223372036854770000,"validity_days":30,"updated_at":"2026-10-10T00:00:00Z","eligibility_status":"READY","recoverable_registration_count":9007199254740993,"unrecoverable_registration_count":0}`;
const body = { enabled: true, amount_points: MAX_BONUS_POINTS, validity_days: 30, expected_revision: '9007199254740993', reason: 'Preserve exact policy revision' };
const memory = () => { const data = new Map(); return { getItem: k => data.get(k) ?? null, setItem: (k,v) => data.set(k,v), removeItem: k => data.delete(k) }; };

test('policy int64 revision, money and readiness counts retain original wire digits', () => {
  const actual = parseRegistrationBonusJSON(policy('9007199254740993'));
  assert.equal(actual.revision, '9007199254740993');
  assert.equal(actual.amount_microcredits, '9223372036854770000');
  assert.equal(actual.recoverable_registration_count, '9007199254740993');
  assert.equal(parseRegistrationBonusJSON(policy('"9223372036854775807"')).revision, '9223372036854775807');
});
test('request emits exact numeric int64 revision while bounded point count stays numeric', () => {
  const encoded = encodeRegistrationBonusJSON(body);
  assert.match(encoded, /"expected_revision":9007199254740993,/);
  assert.match(encoded, /"amount_points":922337203685477,/);
  assert.throws(() => encodeRegistrationBonusJSON({ ...body, amount_points: MAX_BONUS_POINTS + 1 }));
  assert.throws(() => encodeRegistrationBonusJSON({ ...body, expected_revision: 9007199254740992 }));
});
test('invalid revision and malformed successful response fail closed', () => {
  for (const value of ['0', '01', '1.5', '9223372036854775808', null]) assert.throws(() => bonusRevision(value));
  assert.throws(() => parseRegistrationBonusJSON(policy('1.5')));
  assert.throws(() => parseRegistrationBonusJSON(policy('2').replace('"enabled":true', '"enabled":"true"')));
});
test('unknown request preserves the original actor, key, payload and revision across reload', () => {
  const storage = memory();
  const pending = { actor: 'actor-a', key: 'registration-bonus-0199a100-0001-4000-8000-000000000101', body };
  persistBonusPending(pending, storage);
  assert.deepEqual(readBonusPending('actor-a', storage), pending);
  assert.equal(readBonusPending('actor-b', storage), null);
  clearBonusPending('actor-a', storage);
  assert.equal(readBonusPending('actor-a', storage), null);
});
test('unverifiable storage prevents creating recovery evidence', () => {
  assert.throws(() => persistBonusPending({ actor: 'actor-a', key: 'registration-bonus-0199a100-0001-4000-8000-000000000101', body }, { setItem() {}, getItem() { return null; } }));
});
test('a denied replay cannot erase the original unknown receipt, and generic HTTP errors prove nothing', () => {
  for (const error of [{ code: 'RECENT_AUTHENTICATION_REQUIRED', status: 401 }, { code: 'PERMISSION_DENIED', status: 403 }, { code: 'REGISTRATION_BONUS_CONFLICT', status: 409 }]) {
    assert.equal(bonusAttemptProvesNoWrite(error), true);
    assert.equal(bonusAttemptProvesNoWrite(error, true), false);
  }
  assert.equal(bonusAttemptProvesNoWrite({ code: 'REQUEST_FAILED', status: 503 }), false);
  assert.equal(bonusAttemptProvesNoWrite({ code: 'PERMISSION_DENIED', status: 503 }), false);
});
test('client uses authenticated exact GET/PUT path, CSRF and caller-owned stable key', async () => {
  const original = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Response(policy('9007199254740993'), { status: 200 }); };
  try {
    const client = new AdminPlatformClient(); client.csrfToken = 'controlled-csrf';
    await client.registrationBonusPolicy();
    await client.updateRegistrationBonusPolicy(body, 'registration-bonus-controlled-stable');
    assert.ok(calls.every(call => call.url.endsWith('/api/admin/billing/admin/v1/registration-bonus-policy')));
    assert.equal(calls[1].options.method, 'PUT');
    assert.equal(calls[1].options.headers['X-CSRF-Token'], 'controlled-csrf');
    assert.equal(calls[1].options.headers['Idempotency-Key'], 'registration-bonus-controlled-stable');
    assert.match(calls[1].options.body, /"expected_revision":9007199254740993,/);
  } finally { globalThis.fetch = original; }
});
test('all three languages provide every policy and recovery label without fallback', () => {
  const keys = Object.keys(adminPlatformCopy.zh.operations).sort();
  for (const language of ['zh','en','ja']) {
    assert.deepEqual(Object.keys(adminPlatformCopy[language].operations).sort(), keys);
    assert.ok(Object.values(adminPlatformCopy[language].operations).every(value => typeof value === 'string' && value.trim()));
  }
});
