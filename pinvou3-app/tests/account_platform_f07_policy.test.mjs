import assert from 'node:assert/strict';
import test from 'node:test';
import { adminPlatformCopy } from '../src/shared/i18n.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

for (const language of ['zh', 'en', 'ja']) test(`${language}: signup policy has every local authority, recovery and form label`, () => {
  const copy = adminPlatformCopy[language].operations;
  assert.deepEqual(Object.keys(copy).sort(), Object.keys(adminPlatformCopy.zh.operations).sort());
  for (const key of ['title', 'lead', 'enabled', 'amount', 'validityDays', 'lifetimeWarning', 'eligibilityBlocked', 'readOnly', 'reason', 'save', 'reauthenticationDraftIdentityChanged', 'reauthenticationDraftExpired', 'saveReconciled']) assert.ok(typeof copy[key] === 'string' && copy[key].length > 0, key);
  assert.equal(adminPlatformCopy[language].nav.operations, copy.nav);
});

test('Actual policy client uses existing same-origin no-store read and CSRF/key-bound PUT', async () => {
  const original = globalThis.fetch; const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return new Response('{"revision":2,"enabled":false,"amount_microcredits":0,"validity_days":30}', { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AdminPlatformClient(); client.csrfToken = 'controlled-policy-csrf';
    const read = await client.registrationBonusPolicy();
    assert.equal(read.enabled, false);
    const body = { enabled: false, amount_points: 0, validity_days: 30, expected_revision: 1, reason: 'Controlled existing policy edit' };
    await client.updateRegistrationBonusPolicy(body, 'controlled-immutable-policy-key');
    assert.equal(calls.length, 2);
    for (const call of calls) { assert.equal(call.url, '/api/admin/billing/admin/v1/registration-bonus-policy'); assert.equal(call.options.credentials, 'same-origin'); assert.equal(call.options.cache, 'no-store'); }
    assert.equal(calls[0].options.method, undefined);
    assert.equal(calls[1].options.method, 'PUT');
    assert.equal(calls[1].options.headers['X-CSRF-Token'], 'controlled-policy-csrf');
    assert.equal(calls[1].options.headers['Idempotency-Key'], 'controlled-immutable-policy-key');
    assert.deepEqual(JSON.parse(calls[1].options.body), body);
  } finally { globalThis.fetch = original; }
});
