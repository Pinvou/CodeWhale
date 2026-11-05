import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';

test('only a fresh provider-blind action receives protocol provenance; old pending and unrelated actions stay unchanged', () => {
  const source = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AdminPaymentReviewQueue.jsx', import.meta.url), 'utf8');
  const start = source.indexOf('    const scheduleDate =');
  const factsStart = source.indexOf('    const facts = operation.pendingFacts ||', start);
  const end = source.indexOf(';', factsStart);
  assert.ok(start > 0 && end > start);
  const initializer = new Function('operation', 'action', 'detail', 'reason', 'resolution', 'nextReview', `${source.slice(start, end + 1)}\nreturn facts;`);
  for (const action of ['query', 'claim', 'resolve', 'schedule', 'archive', 'provider-blind']) {
    const current = initializer({}, action, { case: { case_id: 'owned-case' } }, ' original reason ', '', '');
    assert.equal(current.commandProtocol, ['schedule', 'provider-blind', 'archive'].includes(action) ? 1 : undefined);
    const pending = Object.freeze({ action, id: 'old-case', reason: 'old original reason' });
    assert.equal(initializer({ pendingFacts: pending }, action, {}, 'new form reason', '', ''), pending);
    assert.equal(pending.commandProtocol, undefined);
  }
});

test('the real review dispatcher and client retain the original provider-blind command identity on explicit recovery', async () => {
  const source = readFileSync(new URL('../../../pinvou3-app/src/features/account-platform/AdminPaymentReviewQueue.jsx', import.meta.url), 'utf8');
  const start = source.indexOf('  const execute =');
  const end = source.indexOf('  const act =', start);
  assert.ok(start > 0 && end > start);
  const execute = new Function('client', 'anomaly', `${source.slice(start, end)}\nreturn execute;`)(new AdminPlatformClient(''), false);
  const facts = Object.freeze({ action: 'provider-blind', id: '11111111-1111-4111-8111-111111111111', reason: 'controlled original provider observation', commandProtocol: 1 });
  const key = 'finance-provider-blind-11111111';
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => {
    calls.push({ url, key: options.headers['Idempotency-Key'], body: options.body });
    if (calls.length === 1) throw new TypeError('controlled committed ACK unavailable');
    return new Response(JSON.stringify({ case_id: facts.id, review_status: 'PROVIDER_BLIND' }), { status: 200 });
  };
  try {
    const oldPending = Object.freeze({ action: facts.action, id: facts.id, reason: facts.reason });
    assert.throws(() => execute(oldPending, key), /FINANCE_COMMAND_PROTOCOL_REQUIRED/);
    assert.equal(calls.length, 0, 'an unversioned old intent must not be upgraded and dispatched');
    await assert.rejects(execute(facts, key), TypeError);
    assert.equal(calls.length, 1, 'the client must not silently dispatch another command');
    assert.equal((await execute(facts, key)).case_id, facts.id);
    assert.deepEqual(calls.map(call => call.key), [key, key]);
    assert.equal(calls[0].body, calls[1].body);
    assert.equal(JSON.parse(calls[0].body).command_protocol, 1);
    assert.equal(calls[0].url, `/api/admin/billing/admin/v1/late-payment-reviews/${facts.id}/provider-blind`);
    await execute(facts, 'finance-provider-blind-new-1111');
    assert.equal(calls[2].key, 'finance-provider-blind-new-1111');
  } finally {
    globalThis.fetch = originalFetch;
  }
});
