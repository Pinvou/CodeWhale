import test from 'node:test';
import assert from 'node:assert/strict';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { reconciliationCommand, reconciliationDayRange, reconciliationImportItems, requireReconciliationDate } from '../src/features/account-platform/reconciliation-contract.js';

const ID = '11111111-1111-4111-8111-111111111111';
test('import preserves int64 source digits and rejects invalid dates', () => {
  assert.equal(reconciliationImportItems('[{"amount_fen":9007199254740993}]')[0].amount_fen, '9007199254740993');
  for (const value of ['2026-02-29', '10/07/2026', '2026-13-01', '2026-99-99']) assert.throws(() => requireReconciliationDate(value), { message: 'INVALID_RECONCILIATION_INPUT' });
  assert.equal(requireReconciliationDate('2024-02-29'), '2024-02-29');
});
test('Shanghai selected days use the next calendar date as exclusive end', () => {
  for (const [from, to] of [['2026-10-07','2026-10-08'],['2026-12-31','2027-01-01'],['2024-02-28','2024-02-29'],['2024-02-29','2024-03-01'],['2026-02-28','2026-03-01']]) {
    assert.deepEqual(reconciliationDayRange(from), { business_date_from: from, business_date_to: to });
  }
  for (const invalid of ['2026-02-29','2026-13-01','10/07/2026','9999-12-31']) assert.throws(() => reconciliationDayRange(invalid), { message: 'INVALID_RECONCILIATION_INPUT' });
});
test('case commands require actual owner/review fields without future financial repair actions', () => {
  const fields = { reason: 'controlled review reason', owner: ID, repair: 'controlled plan', review: 'controlled finding' };
  assert.deepEqual(reconciliationCommand('ASSIGN', fields), { action: 'ASSIGN', reason: fields.reason, owner_id: ID });
  assert.deepEqual(reconciliationCommand('RESOLVE', fields), { action: 'RESOLVE', reason: fields.reason, review_result: fields.review });
  assert.throws(() => reconciliationCommand('CONFIRM_FORCED_REFUND', fields));
  assert.throws(() => reconciliationCommand('ASSIGN', { ...fields, owner: '' }));
});
test('actual client rejects unknown success or wrong run identity facts', async t => {
  const saved = globalThis.fetch; t.after(() => { globalThis.fetch = saved; });
  const client = new AdminPlatformClient(); client.csrfToken = 'public-fixture-csrf';
  const facts = { channel: 'ALIPAY', business_date: '2026-10-07' };
  const run = { reconciliation_run_id: ID, ...facts, status: 'COMPLETED' };
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({url, options}); return new Response(JSON.stringify(run), { status: 200 }); };
  assert.deepEqual(await client.runReconciliation(facts), run);
  assert.equal(calls[0].url, '/api/admin/billing/admin/v1/reconciliation-runs');
  assert.deepEqual(JSON.parse(calls[0].options.body), facts);
  globalThis.fetch = async () => new Response(JSON.stringify(run), { status: 202 });
  await assert.rejects(client.runReconciliation(facts));
  globalThis.fetch = async () => new Response(JSON.stringify({ ...run, business_date: '2026-10-08' }), { status: 200 });
  await assert.rejects(client.runReconciliation(facts));
});
test('case writes preserve the same key and exact money response, reject false ACK', async t => {
  const saved = globalThis.fetch; t.after(() => { globalThis.fetch = saved; });
  const client = new AdminPlatformClient(); client.csrfToken = 'public-fixture-csrf';
  const calls = []; const facts = { action: 'CLAIM', reason: 'controlled claim reason' };
  globalThis.fetch = async (url, options) => { calls.push({url, options}); return new Response(`{"case_id":"${ID}","provider_snapshot":{"amount_fen":9007199254740993}}`, { status: 200 }); };
  for (let i=0;i<2;i++) assert.equal((await client.reconciliationCaseCommand(ID, facts, 'controlled-stable-key')).provider_snapshot.amount_fen, '9007199254740993');
  assert.equal(calls[0].options.headers['Idempotency-Key'], calls[1].options.headers['Idempotency-Key']);
  assert.equal(calls[0].options.body, calls[1].options.body);
  globalThis.fetch = async () => new Response(JSON.stringify({ case_id: '22222222-2222-4222-8222-222222222222' }), { status: 200 });
  await assert.rejects(client.reconciliationCaseCommand(ID, facts, 'controlled-stable-key'));
});
