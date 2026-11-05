import test from 'node:test';
import assert from 'node:assert/strict';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';

for (const [name, create] of [
  ['Desktop', respond => new AccountPlatformDesktopClient({ request: async request => respond(request) })],
  ['BFF', respond => {
    globalThis.fetch = async (url, options) => {
      const resource = url.slice('/api/token'.length);
      const reply = await respond({ area: 'token', resource, method: options.method, body: options.body === undefined ? undefined : JSON.parse(options.body), idempotencyKey: options.headers['Idempotency-Key'], csrf: options.headers['X-CSRF-Token'] });
      return new Response(reply.bodyText ?? JSON.stringify(reply.body), { status: reply.status, headers: { 'Content-Type': 'application/json' } });
    };
    const client = new AccountPlatformBffClient(); client.csrfToken = 'owned-public-csrf-control'; return client;
  }],
]) {
  test(`${name} calls real model catalog and parses exact usage money`, async t => {
    const originalFetch = globalThis.fetch; t.after(() => { globalThis.fetch = originalFetch; });
    const seen = [];
    const client = create(request => { seen.push(request); return request.resource === '/v1/models' ? { status: 200, body: { items: [{ model_id: 'public-model-control' }] } } : { status: 200, bodyText: '{"items":[{"charged_microcredits":9007199254740993}],"next_cursor":"turn-v1.public-next"}' }; });
    assert.equal((await client.models()).items[0].model_id, 'public-model-control');
    assert.equal((await client.usageHistory()).items[0].charged_microcredits, '9007199254740993');
    assert.deepEqual(seen.map(item => item.resource), ['/v1/models', '/v1/me/usage-turns?limit=20']);
  });
  test(`${name} falls back only for initial unavailable turn projection`, async t => {
    const originalFetch = globalThis.fetch; t.after(() => { globalThis.fetch = originalFetch; });
    const seen = []; const client = create(request => { seen.push(request.resource); return seen.length === 1 ? { status: 503, body: { code: 'USAGE_HISTORY_PROJECTION_BUILDING' } } : { status: 200, body: { items: [] } }; });
    assert.deepEqual(await client.usageHistory(), { items: [] });
    assert.deepEqual(seen, ['/v1/me/usage-turns?limit=20', '/v1/me/usage-requests?limit=20']);
  });
  test(`${name} does not replace a paginated turn cursor with request history`, async t => {
    const originalFetch = globalThis.fetch; t.after(() => { globalThis.fetch = originalFetch; });
    const seen = []; const client = create(request => { seen.push(request.resource); return { status: 404, body: { code: 'NOT_FOUND' } }; });
    await assert.rejects(client.usageHistory('turn-v1.page+value'), error => error.status === 404);
    assert.deepEqual(seen, ['/v1/me/usage-turns?limit=20&cursor=turn-v1.page%2Bvalue']);
  });
  test(`${name} preserves legacy cursors and rejects foreign error fallback`, async t => {
    const originalFetch = globalThis.fetch; t.after(() => { globalThis.fetch = originalFetch; });
    const seen = []; const client = create(request => { seen.push(request.resource); return { status: 403, body: { code: 'FORBIDDEN' } }; });
    await assert.rejects(client.usageHistory('request-cursor+1'), error => error.status === 403);
    await assert.rejects(client.usageHistory(), error => error.status === 403);
    assert.deepEqual(seen, ['/v1/me/usage-requests?limit=20&cursor=request-cursor%2B1', '/v1/me/usage-turns?limit=20']);
  });
  test(`${name} cancellation sends actual request ID and same retry intent key`, async t => {
    const originalFetch = globalThis.fetch; t.after(() => { globalThis.fetch = originalFetch; });
    const seen = []; const client = create(request => { seen.push(request); return { status: 200, body: { status: 'DISPLAY_STOPPED_EXECUTION_CONTINUES' } }; });
    await client.cancelUsage('public/id', 'cancel-public-intent-001'); await client.cancelUsage('public/id', 'cancel-public-intent-001');
    assert.equal(seen.length, 2);
    for (const request of seen) { assert.equal(request.area, 'token'); assert.equal(request.resource, '/v1/usage-requests/public%2Fid/cancel'); assert.equal(request.method, 'POST'); assert.deepEqual(request.body, {}); assert.equal(request.idempotencyKey, 'cancel-public-intent-001'); if (name === 'BFF') assert.equal(request.csrf, 'owned-public-csrf-control'); }
  });
}
