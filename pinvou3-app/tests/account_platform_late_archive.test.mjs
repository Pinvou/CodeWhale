import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

const id = '11111111-1111-4111-8111-111111111111';
const facts = Object.freeze({ action: 'archive', id, commandProtocol: 1 });
const key = 'finance-controlled-original-archive';
const queue = await readFile(new URL('../src/features/account-platform/AdminPaymentReviewQueue.jsx', import.meta.url), 'utf8');
const dispatcher = queue.slice(queue.indexOf('  const execute ='), queue.indexOf('  const act ='));
const executeFor = client => new Function('client', 'anomaly', `${dispatcher}\nreturn execute;`)(client, false);
async function withFetch(fetcher, action) {
  const original = globalThis.fetch; globalThis.fetch = fetcher;
  try { await action(); } finally { globalThis.fetch = original; }
}
test('actual archive dispatcher/client reuse the original identity after an unknown acknowledgement', async () => {
  const calls = []; const execute = executeFor(new AdminPlatformClient(''));
  await withFetch(async (url, options) => {
    calls.push({ url, key: options.headers['Idempotency-Key'], body: options.body });
    if (calls.length === 1) throw new TypeError('controlled lost archive ACK');
    return new Response(JSON.stringify({ case_id: id, review_status: 'ARCHIVED_CONFIRMED_UNPAID' }), { status: 200 });
  }, async () => {
    await assert.rejects(execute(facts, key), TypeError);
    assert.equal((await execute(facts, key)).review_status, 'ARCHIVED_CONFIRMED_UNPAID');
  });
  assert.equal(calls.length, 2); assert.deepEqual(calls[1], calls[0]); assert.equal(calls[0].key, key);
  assert.deepEqual(JSON.parse(calls[0].body), {});
});
test('each new logical archive retains its own identity and exact case', async () => {
  const calls = []; const execute = executeFor(new AdminPlatformClient(''));
  await withFetch(async (url, options) => {
    calls.push({ url, key: options.headers['Idempotency-Key'] });
    return new Response(JSON.stringify({ case_id: id }), { status: 200 });
  }, async () => { await execute(facts, key); await execute({ ...facts, id: '22222222-2222-4222-8222-222222222222' }, 'finance-controlled-other-archive'); });
  assert.notEqual(calls[0].key, calls[1].key); assert.notEqual(calls[0].url, calls[1].url);
});
test('archive key admission is declared undispatched before fetch', async () => {
  let calls = 0;
  await withFetch(() => { calls++; throw new Error('unexpected fetch'); }, async () => {
    for (const invalid of ['', undefined, 'short', 'k'.repeat(121)]) assert.throws(() => new AdminPlatformClient('').archiveLatePaymentReview(id, invalid, 1), error => error.financeRequestState === 'NOT_DISPATCHED');
  });
  assert.equal(calls, 0);
});
test('an archive conflict is never converted to a successful original receipt', async () => {
  await withFetch(async () => new Response(JSON.stringify({ code: 'CONFLICT' }), { status: 409 }), async () => {
    await assert.rejects(executeFor(new AdminPlatformClient(''))(facts, key), error => error.status === 409 && error.financeRequestState !== 'NOT_DISPATCHED');
  });
});

const actSource = queue.slice(queue.indexOf('  const act ='), queue.indexOf('  const item ='));
const actFor = (operation, execute = () => {}) => new Function('operation', 'detail', 'reason', 'resolution', 'nextReview', 'paymentAnomalyResolutions', 'anomaly', 'copy', 'execute', 'open', 'load', `${actSource}\nreturn act;`)(operation, { case: { case_id: id, anomaly_type: 'controlled' } }, 'controlled review reason', 'controlled', '2099-01-01T00:00:00Z', () => ['controlled'], false, { confirmAction: 'controlled confirmation' }, execute, async () => {}, async () => {});

test('actual fresh facts mark the three upgraded actions and leave other actions unmarked', async () => {
  const previous = globalThis.confirm; globalThis.confirm = () => true;
  try {
    for (const action of ['schedule', 'archive', 'provider-blind', 'query', 'claim', 'resolve']) {
      let captured;
      const operation = { pendingFacts: null, unknown: false, mutate: async value => { captured = value; } };
      await actFor(operation)(action);
      assert.equal(captured.commandProtocol, ['schedule', 'provider-blind', 'archive'].includes(action) ? 1 : undefined);
      assert.equal(Object.hasOwn(JSON.parse(JSON.stringify(captured)), 'commandProtocol'), ['schedule', 'provider-blind', 'archive'].includes(action));
    }
  } finally { globalThis.confirm = previous; }
});

test('legacy pending facts stay original and missing protocol is never defaulted before dispatch', async () => {
  const legacy = { ...facts }; delete legacy.commandProtocol;
  const captured = []; let rejection; let calls = 0;
  await withFetch(() => { calls++; throw new Error('unexpected legacy retry'); }, async () => {
    const operation = { pendingFacts: legacy, unknown: true, mutate: async (current, callback) => {
      assert.equal(current, legacy); captured.push(current);
      try { await callback(current, key); } catch (error) { rejection = error; }
    } };
    await actFor(operation, executeFor(new AdminPlatformClient('')))('archive');
  });
  assert.equal(calls, 0); assert.equal(captured[0], legacy); assert.equal(Object.hasOwn(legacy, 'commandProtocol'), false);
  assert.equal(rejection.financeRequestState, 'NOT_DISPATCHED');
});

test('the client requires explicit protocol number one without a default or coercion', async () => {
  let calls = 0;
  await withFetch(() => { calls++; throw new Error('unexpected invalid protocol dispatch'); }, async () => {
    for (const commandProtocol of [undefined, null, 0, 2, '1', true]) {
      assert.throws(() => new AdminPlatformClient('').archiveLatePaymentReview(id, key, commandProtocol), error => error.financeRequestState === 'NOT_DISPATCHED');
    }
  });
  assert.equal(calls, 0);
});
