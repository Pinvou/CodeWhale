import assert from 'node:assert/strict';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { freshLatePaymentQuery, latePaymentQueryAuthority } from '../src/features/account-platform/late-payment-query-contract.js';

const id = '11111111-1111-4111-8111-111111111111';
const other = '22222222-2222-4222-8222-222222222222';
const item = { case_id: id, payment_order_id: other, provider_id: 'controlled', provider_version: 'v1', merchant_order_no: 'controlled-order' };
const authority = version => latePaymentQueryAuthority({ user_id: id, active_context: { context_version: 1, personal_authorization_version: 1, platform_authorization_version: version, organization_authorization_version: 0 } });
const facts = freshLatePaymentQuery(item, id, authority(1));
const key = 'finance-controlled-original-query';
const completed = () => ({ command_protocol: 1, query_reference: key, actor_id: id, case_id: id,
  payment_order_id: other, state: 'COMPLETED', result: { ...item, last_query_at: '2026-10-25T00:00:00Z', last_query_result: 'PENDING' } });

for (const [method, action] of [['queryLatePaymentReview', 'query-commands'], ['queryLatePaymentReviewResult', 'query-command-results']]) {
  test(`actual client ${method} keeps exact original facts and key on its distinct route`, async t => {
    const original = globalThis.fetch;
    t.after(() => { globalThis.fetch = original; });
    const client = new AdminPlatformClient(''); client.csrfToken = 'PUBLIC_SYNTHETIC_CSRF';
    const calls = [];
    globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Response(JSON.stringify(completed()), { status: 200 }); };
    assert.deepEqual(await client[method](id, facts, key), completed().result);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, `/api/admin/billing/admin/v1/late-payment-reviews/${id}/${action}`);
    const options = calls[0].options;
    assert.equal(options.headers['Idempotency-Key'], key);
    assert.equal(options.headers['X-CSRF-Token'], client.csrfToken);
    assert.deepEqual(JSON.parse(options.body), { command_protocol: 1, payment_order_id: other,
      provider_id: 'controlled', provider_version: 'v1', merchant_order_no: 'controlled-order' });
    assert.equal(options.credentials, 'same-origin');
  });
}

test('legacy pending and malformed original query cannot receive a protocol or dispatch', async t => {
  const original = globalThis.fetch; t.after(() => { globalThis.fetch = original; });
  let calls = 0;
  globalThis.fetch = async () => { calls++; throw new Error('unexpected fetch'); };
  const client = new AdminPlatformClient('');
  for (const originalFacts of [undefined, { action: 'query', id }, { ...facts, commandProtocol: 2 }, { ...facts, id: other }]) {
    for (const method of ['queryLatePaymentReview', 'queryLatePaymentReviewResult']) {
      await assert.rejects(client[method](id, originalFacts, key), error => error.financeRequestState === 'NOT_DISPATCHED');
    }
  }
  assert.equal(calls, 0);
});

test('UNKNOWN, current terminal projections and mismatched receipts never confirm the original query', async t => {
  const original = globalThis.fetch; t.after(() => { globalThis.fetch = original; });
  const client = new AdminPlatformClient('');
  for (const receipt of [{ ...item, review_status: 'RESOLVED_PAID' }, { ...completed(), state: 'UNKNOWN', result: undefined },
    { ...completed(), query_reference: 'different-original-key' }, { ...completed(), actor_id: other },
    { ...completed(), payment_order_id: id }, { ...completed(), command_protocol: undefined },
    { ...completed(), result: { ...completed().result, last_query_at: undefined } }]) {
    globalThis.fetch = async () => new Response(JSON.stringify(receipt), { status: 200 });
    await assert.rejects(client.queryLatePaymentReviewResult(id, facts, key), error => error.code === 'COMMAND_RESULT_UNKNOWN');
  }
});

test('retired session completion cannot confirm a query in the new session', async t => {
  const original = globalThis.fetch; t.after(() => { globalThis.fetch = original; });
  const client = new AdminPlatformClient('');
  let release;
  globalThis.fetch = () => new Promise(resolve => { release = resolve; });
  const pending = client.queryLatePaymentReviewResult(id, facts, key);
  client.retireSession();
  release(new Response(JSON.stringify(completed()), { status: 200 }));
  await assert.rejects(pending);
});

for (const method of ['queryLatePaymentReview', 'queryLatePaymentReviewResult']) {
  for (const phase of ['before headers', 'own headers', 'during body', 'first baseline then change']) {
    test(`${method} rejects authorization retirement ${phase}`, async t => {
      const original = globalThis.fetch;
      t.after(() => { globalThis.fetch = original; });
      const client = new AdminPlatformClient('');
      const observe = version => client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': String(version) }));
      if (phase !== 'first baseline then change') observe(1);
      let release;
      let body;
      let calls = 0;
      globalThis.fetch = () => {
        calls++;
        if (phase === 'during body') {
          return Promise.resolve(new Response(new ReadableStream({ start(controller) { body = controller; } }), {
            status: 200, headers: { 'X-Platform-Authorization-Version': '1' },
          }));
        }
        return new Promise(resolve => { release = resolve; });
      };
      const pending = client[method](id, facts, key);
      if (phase === 'during body') {
        await new Promise(resolve => setImmediate(resolve));
        observe(2);
        body.enqueue(new TextEncoder().encode(JSON.stringify(completed()))); body.close();
      } else {
        if (phase === 'first baseline then change') observe(1);
        if (phase !== 'own headers') observe(2);
        release(new Response(JSON.stringify(completed()), { status: 200,
          headers: { 'X-Platform-Authorization-Version': phase === 'own headers' ? '2' : '1' } }));
      }
      await assert.rejects(pending, error => error.name === 'AbortError');
      assert.equal(calls, 1);
    });
  }
  for (const baseline of ['first observation', 'same version']) {
    test(`${method} accepts ${baseline} and exact receipt`, async t => {
      const original = globalThis.fetch; t.after(() => { globalThis.fetch = original; });
      const client = new AdminPlatformClient('');
      if (baseline === 'same version') client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': '1' }));
      globalThis.fetch = async () => new Response(JSON.stringify(completed()), { status: 200,
        headers: { 'X-Platform-Authorization-Version': '1' } });
      assert.deepEqual(await client[method](id, facts, key), completed().result);
    });
  }
}

for (const method of ['queryLatePaymentReview', 'queryLatePaymentReviewResult']) {
  test(`${method} rejects first-call stale nested me before fetch after real observer change`, async t => {
    const original = globalThis.fetch; t.after(() => { globalThis.fetch = original; });
    const client = new AdminPlatformClient('');
    for (const version of [1, 2]) client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': String(version) }));
    let calls = 0; globalThis.fetch = async () => { calls++; return new Response(JSON.stringify(completed())); };
    await assert.rejects(client[method](id, facts, key), error => error.financeRequestState === 'NOT_DISPATCHED' && error.code === 'QUERY_AUTHORIZATION_CHANGED');
    assert.equal(calls, 0);
  });
  test(`${method} accepts current nested me with exact original wire contract`, async t => {
    const original = globalThis.fetch; t.after(() => { globalThis.fetch = original; });
    const client = new AdminPlatformClient('');
    for (const version of [1, 2]) client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': String(version) }));
    const current = freshLatePaymentQuery(item, id, authority(2)); const calls = [];
    globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Response(JSON.stringify(completed()), { headers: { 'X-Platform-Authorization-Version': '2' } }); };
    assert.deepEqual(await client[method](id, current, key), completed().result);
    assert.equal(calls.length, 1);
    assert.deepEqual(JSON.parse(calls[0].options.body), { command_protocol: 1, payment_order_id: other, provider_id: 'controlled', provider_version: 'v1', merchant_order_no: 'controlled-order' });
    assert.equal(calls[0].options.headers['Idempotency-Key'], key);
  });
}

test('current lawful authority only reads original key/facts without replacing original authority or dispatch route', async t => {
  const original = globalThis.fetch; t.after(() => { globalThis.fetch = original; });
  const client = new AdminPlatformClient('');
  for (const version of [1, 2]) client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': String(version) }));
  const prior = JSON.stringify(facts); const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Response(JSON.stringify(completed()), { headers: { 'X-Platform-Authorization-Version': '2' } }); };
  assert.deepEqual(await client.queryLatePaymentReviewResult(id, facts, key, undefined, authority(2)), completed().result);
  assert.equal(JSON.stringify(facts), prior); assert.ok(Object.isFrozen(facts.authorization));
  assert.equal(calls.length, 1); assert.ok(calls[0].url.endsWith('/query-command-results'));
  assert.equal(calls[0].options.headers['Idempotency-Key'], key);
});
