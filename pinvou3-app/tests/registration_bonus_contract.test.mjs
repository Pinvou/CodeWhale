import test from 'node:test';
import assert from 'node:assert/strict';
import { parseRegistrationBonusJSON, encodeRegistrationBonusJSON, bonusRevision, MAX_BONUS_POINTS, registrationBonusReasonIsValid, registrationBonusFormIsValid } from '../src/features/account-platform/registration-bonus-contract.js';
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

test('reason bounds use trimmed Unicode code points, matching the existing wire contract', () => {
  for (const [reason, valid] of [
    ['😀'.repeat(5), false], ['😀'.repeat(9), false],
    ['😀'.repeat(10), true], ['😀'.repeat(500), true], ['😀'.repeat(501), false],
    ['  ' + '😀'.repeat(9) + 'a  ', true], ['  ' + 'a'.repeat(9) + '  ', false],
    ['', false], [null, false], [10, false],
  ]) {
    assert.equal(registrationBonusReasonIsValid(reason), valid);
    if (valid) assert.equal(JSON.parse(encodeRegistrationBonusJSON({ ...body, reason })).reason, reason.trim());
    else assert.throws(() => encodeRegistrationBonusJSON({ ...body, reason }), /INVALID_REGISTRATION_BONUS_REQUEST/);
  }
});

test('new policy form requires explicit bounded numbers and a complete reason while preserving zero', () => {
  const form = { enabled: true, amount_points: '0', validity_days: '30', reason: body.reason };
  assert.equal(registrationBonusFormIsValid(form), true);
  assert.equal(registrationBonusFormIsValid({ ...form, amount_points: String(MAX_BONUS_POINTS), validity_days: '3650' }), true);
  assert.equal(registrationBonusFormIsValid({ ...form, amount_points: ' 0 ', validity_days: ' 1 ' }), true);
  for (const amount_points of ['', ' ', '\t\n', '-1', '0.5', String(MAX_BONUS_POINTS + 1), '9007199254740992', 'Infinity', 'NaN', null, 0]) {
    assert.equal(registrationBonusFormIsValid({ ...form, amount_points }), false, `amount ${JSON.stringify(amount_points)}`);
  }
  for (const validity_days of ['', ' ', '0', '-1', '1.5', '3651', 'Infinity', null, 30]) {
    assert.equal(registrationBonusFormIsValid({ ...form, validity_days }), false, `validity ${JSON.stringify(validity_days)}`);
  }
  for (const reason of ['', ' '.repeat(10), 'a'.repeat(9), '😀'.repeat(501)]) {
    assert.equal(registrationBonusFormIsValid({ ...form, reason }), false);
  }
  assert.equal(registrationBonusFormIsValid({ ...form, reason: '😀'.repeat(10) }), true);
  assert.equal(registrationBonusFormIsValid({ ...form, enabled: 'true' }), false);
  assert.equal(registrationBonusFormIsValid(null), false);
});

test('legal non-BMP recovery preserves the original actor, request key and complete reason', () => {
  const storage = memory();
  const pending = { actor: 'actor-a', key: 'registration-bonus-0199a100-0001-4000-8000-000000000101', body: { ...body, reason: '😀'.repeat(500) } };
  persistBonusPending(pending, storage);
  assert.deepEqual(readBonusPending('actor-a', storage), pending);
  assert.equal(readBonusPending('actor-b', storage), null);
  assert.equal([...JSON.parse(encodeRegistrationBonusJSON(readBonusPending('actor-a', storage).body)).reason].length, 500);
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
