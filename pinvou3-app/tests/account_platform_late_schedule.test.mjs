import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

const id = '11111111-1111-4111-8111-111111111111';
const facts = Object.freeze({ action: 'schedule', id, commandProtocol: 1, nextReview: '2026-10-26T12:34:56.123Z', reason: 'controlled original review reason' });
const key = 'finance-controlled-original-key';
const queue = await readFile(new URL('../src/features/account-platform/AdminPaymentReviewQueue.jsx', import.meta.url), 'utf8');
// Run the shipped dispatcher rather than a separately written schedule callback.
const dispatcher = queue.slice(queue.indexOf('  const execute ='), queue.indexOf('  const act ='));
const executeFor = client => new Function('client', 'anomaly', `${dispatcher}\nreturn execute;`)(client, false);
async function withFetch(fetcher, action) {
  const original = globalThis.fetch;
  globalThis.fetch = fetcher;
  try { await action(); } finally { globalThis.fetch = original; }
}

test('schedule dispatcher retains the original command key and facts on an unknown retry', async () => {
  const requests = [];
  const execute = executeFor(new AdminPlatformClient(''));
  await withFetch(async (url, options) => {
    requests.push({ url, body: options.body, key: options.headers['Idempotency-Key'] });
    if (requests.length === 1) throw new TypeError('controlled lost acknowledgement');
    return new Response(JSON.stringify({ case_id: id, next_review_at: facts.nextReview }), { status: 200 });
  }, async () => {
    await assert.rejects(execute(facts, key), TypeError);
    const original = await execute(facts, key);
    assert.equal(original.case_id, id);
  });
  assert.equal(requests.length, 2);
  assert.deepEqual(requests[1], requests[0]);
  assert.equal(requests[0].key, key);
  assert.deepEqual(JSON.parse(requests[0].body), { command_protocol: 1, next_review_at: facts.nextReview, reason: facts.reason });
});

test('a new logical schedule carries its new identity and new plan', async () => {
  const requests = [];
  const execute = executeFor(new AdminPlatformClient(''));
  await withFetch(async (_url, options) => {
    requests.push({ key: options.headers['Idempotency-Key'], body: JSON.parse(options.body) });
    return new Response(JSON.stringify({ case_id: id }), { status: 200 });
  }, async () => {
    await execute(facts, key);
    await execute({ ...facts, nextReview: '2026-10-27T12:34:56.123Z', reason: 'controlled later review reason' }, 'finance-controlled-new-key');
  });
  assert.notEqual(requests[0].key, requests[1].key);
  assert.notDeepEqual(requests[0].body, requests[1].body);
});

test('invalid schedule identity or date is declared undispatched before any fetch', async () => {
  let calls = 0;
  await withFetch(() => { calls += 1; throw new Error('unexpected request'); }, async () => {
    for (const invalid of ['', undefined, 'short', 'k'.repeat(121)]) {
      assert.throws(() => new AdminPlatformClient('').scheduleLatePaymentReview(id, facts.nextReview, facts.reason, invalid, 1), error => error.financeRequestState === 'NOT_DISPATCHED');
    }
    assert.throws(() => new AdminPlatformClient('').scheduleLatePaymentReview(id, 'invalid-date', facts.reason, key, 1), error => error.financeRequestState === 'NOT_DISPATCHED');
  });
  assert.equal(calls, 0);
});

test('a conflict remains an unresolved server outcome instead of undispatched evidence', async () => {
  await withFetch(async () => new Response(JSON.stringify({ code: 'CONFLICT' }), { status: 409 }), async () => {
    await assert.rejects(executeFor(new AdminPlatformClient(''))(facts, key), error => error.status === 409 && error.financeRequestState !== 'NOT_DISPATCHED');
  });
});

const actSource = queue.slice(queue.indexOf('  const act ='), queue.indexOf('  const item ='));
const actFor = (operation, execute = () => {}, localReview = '2099-01-01T00:00:00Z') => new Function('operation', 'detail', 'reason', 'resolution', 'nextReview', 'paymentAnomalyResolutions', 'anomaly', 'copy', 'execute', 'open', 'load', `${actSource}\nreturn act;`)(operation, { case: { case_id: id, anomaly_type: 'controlled' } }, 'controlled review reason', 'controlled', localReview, () => ['controlled'], false, { confirmAction: 'controlled confirmation' }, execute, async () => {}, async () => {});

test('actual fresh facts mark the two upgraded actions and leave unrelated actions unmarked', async () => {
  const previous = globalThis.confirm; globalThis.confirm = () => true;
  try {
    for (const action of ['schedule', 'archive', 'provider-blind', 'query', 'claim', 'resolve']) {
      let captured;
      const operation = { pendingFacts: null, unknown: false, mutate: async value => { captured = value; } };
      await actFor(operation)(action);
      assert.equal(captured.commandProtocol, ['schedule', 'provider-blind'].includes(action) ? 1 : undefined);
      assert.equal(Object.hasOwn(JSON.parse(JSON.stringify(captured)), 'commandProtocol'), ['schedule', 'provider-blind'].includes(action));
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
    await actFor(operation, executeFor(new AdminPlatformClient('')))('schedule');
  });
  assert.equal(calls, 0); assert.equal(captured[0], legacy); assert.equal(Object.hasOwn(legacy, 'commandProtocol'), false);
  assert.equal(rejection.financeRequestState, 'NOT_DISPATCHED');
});

test('the client requires explicit protocol number one without a default or coercion', async () => {
  let calls = 0;
  await withFetch(() => { calls++; throw new Error('unexpected invalid protocol dispatch'); }, async () => {
    for (const commandProtocol of [undefined, null, 0, 2, '1', true]) {
      assert.throws(() => new AdminPlatformClient('').scheduleLatePaymentReview(id, facts.nextReview, facts.reason, key, commandProtocol), error => error.financeRequestState === 'NOT_DISPATCHED');
    }
  });
  assert.equal(calls, 0);
});


test('fresh schedule retains its first UTC instant across time-zone changes', async () => {
  const priorTZ = process.env.TZ;
  const priorConfirm = globalThis.confirm;
  globalThis.confirm = () => true;
  try {
    for (const initial of ['Etc/UTC', 'Asia/Tokyo']) {
      process.env.TZ = initial;
      const local = new Date(Date.now() + 72 * 60 * 60 * 1000).toISOString().slice(0, 16);
      const expected = new Date(local).toISOString();
      const requests = [];
      let retained;
      const operation = { pendingFacts: null, unknown: false, mutate: async (current, callback) => {
        retained ||= structuredClone(current);
        operation.pendingFacts = retained; operation.unknown = true;
        try { return await callback(retained, key); } catch (error) { assert.ok(error instanceof TypeError); return null; }
      } };
      await withFetch(async (_url, options) => {
        requests.push({ body: options.body, key: options.headers['Idempotency-Key'] });
        if (requests.length === 1) throw new TypeError('controlled committed schedule ACK lost');
        return new Response(JSON.stringify({ case_id: id }), { status: 200 });
      }, async () => {
        await actFor(operation, executeFor(new AdminPlatformClient('')), local)('schedule');
        assert.equal(retained.nextReview, expected);
        assert.equal(JSON.parse(requests[0].body).next_review_at, expected);
        process.env.TZ = initial === 'Etc/UTC' ? 'Asia/Tokyo' : 'Etc/UTC';
        await actFor(operation, executeFor(new AdminPlatformClient('')), 'different current form')('schedule');
      });
      assert.equal(requests.length, 2);
      assert.deepEqual(requests[1], requests[0]);
      assert.equal(retained.nextReview, expected);
    }
  } finally {
    if (priorTZ === undefined) delete process.env.TZ; else process.env.TZ = priorTZ;
    globalThis.confirm = priorConfirm;
  }
});

test('ambiguous retained local time is never guessed or redispatched', async () => {
  const pending = Object.freeze({ ...facts, nextReview: new Date(Date.now() + 72 * 60 * 60 * 1000).toISOString().slice(0, 16) });
  let calls = 0; let failure;
  const operation = { pendingFacts: pending, unknown: true, mutate: async (current, callback) => {
    assert.equal(current, pending);
    try { await callback(current, key); } catch (error) { failure = error; }
  } };
  await withFetch(() => { calls++; throw new Error('ambiguous retry must not dispatch'); }, async () => {
    await actFor(operation, executeFor(new AdminPlatformClient('')))('schedule');
  });
  assert.equal(calls, 0);
  assert.equal(operation.pendingFacts, pending);
  assert.equal(operation.unknown, true);
  assert.equal(failure.financeRequestState, 'NOT_DISPATCHED');
});

test('explicit-offset client input remains one absolute instant in another zone', async () => {
  const priorTZ = process.env.TZ;
  const instant = Date.now() + 72 * 60 * 60 * 1000;
  const offset = new Date(instant + 9 * 60 * 60 * 1000).toISOString().replace('Z', '+09:00');
  const requests = [];
  try {
    await withFetch(async (_url, options) => {
      requests.push(options.body); return new Response(JSON.stringify({ case_id: id }), { status: 200 });
    }, async () => {
      for (const zone of ['Etc/UTC', 'Asia/Tokyo']) {
        process.env.TZ = zone;
        await new AdminPlatformClient('').scheduleLatePaymentReview(id, offset, facts.reason, key, 1);
      }
    });
    assert.equal(requests[1], requests[0]);
    assert.equal(JSON.parse(requests[0]).next_review_at, new Date(instant).toISOString());
  } finally {
    if (priorTZ === undefined) delete process.env.TZ; else process.env.TZ = priorTZ;
  }
});
