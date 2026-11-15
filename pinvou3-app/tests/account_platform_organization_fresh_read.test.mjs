import test from 'node:test';
import assert from 'node:assert/strict';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { OrganizationAdminClient } from '../src/features/account-platform/organization-client.js';

const ORG = '019aa001-0000-7000-8000-000000000001';
const OTHER = '019aa001-0000-7000-8000-000000000002';
const CREATED = '019aa001-0000-7000-8000-000000000003';
const USER = '019aa001-0000-7000-8000-000000000004';
const OLD = '账期 & A Controlled Old', NEW = '账期 & A Controlled New';
const ROOT = '/api/admin/account/admin/v1/organizations';
const deferred = () => { let resolve; const promise = new Promise(accept => { resolve = accept; }); return { promise, resolve }; };

// Real API classes, coalescing, abort/session fences and Response.json decoding.
// Only the external fetch/JSON completion is controlled: no HTTP server, DB,
// browser, replicated inflight Map, fake API client or wrapper-generation model.
async function withReads(action) {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'fetch');
  const calls = [], tasks = [], clients = [];
  let name = OLD, version = 1, created = false, nextStatus = 200;
  const profile = id => ({ organization_id: id, name: id === ORG ? name : id === CREATED ? '账期 & A Z Created' : '账期 & A Other', status: 'ACTIVE', version: id === ORG ? version : 1 });
  const row = id => { const p = profile(id); return { organization_id: p.organization_id, name: p.name, status: p.status, member_count: id === CREATED ? 1 : 2, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-11-01T00:00:00Z' }; };
  const page = () => ({ items: [row(ORG), row(OTHER), ...(created ? [row(CREATED)] : [])], next_cursor: null });
  const track = promise => { tasks.push(promise); void promise.catch(() => {}); return promise; };
  const gets = () => calls.filter(call => call.method === 'GET');
  const writes = () => calls.filter(call => call.method !== 'GET');
  try {
    Object.defineProperty(globalThis, 'fetch', { configurable: true, writable: true, value: async (url, options = {}) => {
      const method = options.method || 'GET', parsed = new URL(url, 'https://controlled.invalid');
      if (method !== 'GET') {
        const body = JSON.parse(options.body);
        calls.push({ url, method, options, body });
        assert.equal(options.headers['X-CSRF-Token'], 'controlled-csrf');
        if (method === 'PATCH') {
          assert.equal(parsed.pathname, ROOT + '/' + ORG);
          assert.deepEqual(body, { name: NEW });
          assert.equal(options.headers['Idempotency-Key'], 'controlled-rename-key');
          name = NEW; version++;
          return new Response(null, { status: 204 });
        }
        assert.equal(method, 'POST'); assert.equal(parsed.pathname, ROOT);
        assert.deepEqual(body, { name: '账期 & A Z Created', owner_user_id: USER });
        assert.equal(options.headers['Idempotency-Key'], 'controlled-create-key');
        created = true;
        return new Response(JSON.stringify(profile(CREATED)), { status: 201, headers: { 'Content-Type': 'application/json' } });
      }
      const status = parsed.searchParams.has('cursor') ? 400 : nextStatus;
      nextStatus = 200;
      const snapshot = status >= 400 ? { code: 'REQUEST_FAILED', message: 'Controlled read rejection' } : parsed.pathname === ROOT ? page() : profile(parsed.pathname.split('/').pop());
      const gate = deferred(), started = deferred();
      const response = new Response(JSON.stringify(snapshot), { status, headers: { 'Content-Type': 'application/json' } });
      const decode = response.json.bind(response);
      // Gate only completion; delegate bytes and JSON parsing to real Response.
      response.json = async () => { started.resolve(); await gate.promise; return decode(); };
      calls.push({ url, method, options, snapshot, started: started.promise, release: () => gate.resolve() });
      return response;
    } });
    const make = () => { const client = new AdminPlatformClient(); client.csrfToken = 'controlled-csrf'; clients.push(client); return { client, api: new OrganizationAdminClient(client) }; };
    const pair = make();
    await action({ ...pair, make, track, gets, writes, failNext: () => { nextStatus = 500; }, rename: api => track(api.write(ORG, 'rename', { name: NEW }, 'controlled-rename-key')) });
  } finally {
    for (const client of clients) client.retireSession();
    for (const call of gets()) call.release();
    await Promise.allSettled(tasks);
    if (previous) Object.defineProperty(globalThis, 'fetch', previous); else delete globalThis.fetch;
  }
}

const currentName = value => value.items.find(item => item.organization_id === ORG)?.name;
const options = { concurrency: false, timeout: 10000 };

test('Old shared-read configuration really returns the pre-ACK directory snapshot', options, async () => {
  await withReads(async h => {
    const before = h.track(h.api.list()); await h.gets()[0].started;
    assert.equal(await h.rename(h.api), null);
    const after = h.track(h.api.list());
    assert.equal(h.gets().length, 1);
    h.gets()[0].release();
    assert.equal(currentName(await before), OLD); assert.equal(currentName(await after), OLD);
    assert.equal(h.writes().length, 1);
  });
});

test('Post-ACK fresh list starts a real new GET; old JSON and cleanup cannot replace or remove its entry', options, async () => {
  await withReads(async h => {
    const before = h.track(h.api.list()); await h.gets()[0].started;
    assert.equal(await h.rename(h.api), null);
    const after = h.track(h.api.list({}, undefined, { fresh: true })); await h.gets()[1].started;
    assert.equal(h.gets().length, 2); assert.equal(h.gets()[0].url, ROOT + '?limit=25');
    assert.equal(h.gets()[1].url, ROOT + '?limit=25'); assert.equal(h.gets()[0].options.signal.aborted, true);
    h.gets()[0].release(); await assert.rejects(before, { name: 'AbortError' });
    const sharedFresh = h.track(h.api.list()); assert.equal(h.gets().length, 2, 'retired cleanup must not remove the new entry');
    h.gets()[1].release(); assert.equal(currentName(await after), NEW); assert.equal(currentName(await sharedFresh), NEW);
    const next = h.track(h.api.list()); await h.gets()[2].started;
    assert.equal(h.gets().length, 3, 'settled entry must not lock the next read');
    h.gets()[2].release(); assert.equal(currentName(await next), NEW); assert.equal(h.writes().length, 1);
  });
});

test('Fresh failure does not fall back to the old page; a later read recovers without another write', options, async () => {
  await withReads(async h => {
    const before = h.track(h.api.list()); await h.gets()[0].started; await h.rename(h.api);
    h.failNext(); const failed = h.track(h.api.list({}, undefined, { fresh: true })); await h.gets()[1].started;
    h.gets()[1].release(); await assert.rejects(failed, error => error.status === 500);
    const recovered = h.track(h.api.list({}, undefined, { fresh: true })); await h.gets()[2].started;
    h.gets()[2].release(); assert.equal(currentName(await recovered), NEW);
    h.gets()[0].release(); await assert.rejects(before, { name: 'AbortError' });
    assert.equal(h.writes().length, 1);
  });
});

test('Create201 uses the same post-write directory boundary without optimistic insertion', options, async () => {
  await withReads(async h => {
    const before = h.track(h.api.list()); await h.gets()[0].started;
    const result = await h.track(h.api.write('', 'create', { name: '账期 & A Z Created', owner_user_id: USER }, 'controlled-create-key'));
    assert.equal(result.organization_id, CREATED); assert.equal(result.version, 1);
    const after = h.track(h.api.list({}, undefined, { fresh: true })); await h.gets()[1].started;
    h.gets()[1].release(); assert.equal((await after).items.find(item => item.organization_id === CREATED)?.name, '账期 & A Z Created');
    h.gets()[0].release(); await assert.rejects(before, { name: 'AbortError' }); assert.equal(h.writes().length, 1);
  });
});

test('Fresh summary retains canonical decoding and recovers after a foreign-profile response', options, async () => {
  await withReads(async h => {
    const before = h.track(h.api.summary(ORG)); await h.gets()[0].started; await h.rename(h.api);
    const after = h.track(h.api.summary(ORG, undefined, { fresh: true })); await h.gets()[1].started;
    h.gets()[1].release(); assert.equal((await after).name, NEW);
    h.gets()[0].release(); await assert.rejects(before, { name: 'AbortError' });
    // Fault injection is a valid organization DTO for a different target.
    const fetch = globalThis.fetch;
    globalThis.fetch = async (url, init) => url === ROOT + '/' + ORG ? new Response(JSON.stringify({ organization_id: OTHER, name: 'Other', status: 'ACTIVE', version: 1 }), { headers: { 'Content-Type': 'application/json' } }) : fetch(url, init);
    try { await assert.rejects(h.track(h.api.summary(ORG, undefined, { fresh: true })), /INVALID_ORGANIZATION_ACK/); }
    finally { globalThis.fetch = fetch; }
    const recovered = h.track(h.api.summary(ORG, undefined, { fresh: true })); await h.gets()[2].started;
    h.gets()[2].release(); assert.equal((await recovered).name, NEW);
  });
});

test('Fresh invalidation uses the exact encoded q/status/opaque cursor and does not cancel other resources', options, async () => {
  await withReads(async h => {
    // This is URI forwarding and rejected-cursor handling, not a claim that a
    // synthetic opaque cursor is accepted by the authenticated server codec.
    const filters = { q: '账期 & A', status: 'ACTIVE', cursor: 'controlled_cursor-2' };
    const before = h.track(h.api.list(filters)); await h.gets()[0].started;
    const otherQuery = h.track(h.api.list({ q: '账期 & A', status: 'ACTIVE' })); await h.gets()[1].started;
    const otherOrg = h.track(h.api.summary(OTHER)); await h.gets()[2].started;
    const after = h.track(h.api.list(filters, undefined, { fresh: true })); await h.gets()[3].started;
    const expected = ROOT + '?limit=25&q=%E8%B4%A6%E6%9C%9F+%26+A&status=ACTIVE&cursor=controlled_cursor-2';
    assert.equal(h.gets()[0].url, expected); assert.equal(h.gets()[3].url, expected);
    assert.equal(h.gets()[0].options.signal.aborted, true); assert.equal(h.gets()[1].options.signal.aborted, false); assert.equal(h.gets()[2].options.signal.aborted, false);
    h.gets()[0].release(); await assert.rejects(before, { name: 'AbortError' });
    h.gets()[3].release(); await assert.rejects(after, error => error.status === 400);
    h.gets()[1].release(); assert.equal(currentName(await otherQuery), OLD);
    h.gets()[2].release(); assert.equal((await otherOrg).organization_id, OTHER);
  });
});

test('Pre-aborted fresh callers do not cancel healthy reads, and invalidation stays client-local', options, async () => {
  await withReads(async h => {
    const before = h.track(h.api.list()); await h.gets()[0].started;
    const cancelled = new AbortController(); cancelled.abort();
    await assert.rejects(h.track(h.api.list({}, cancelled.signal, { fresh: true })), { name: 'AbortError' });
    assert.equal(h.gets().length, 1); assert.equal(h.gets()[0].options.signal.aborted, false);
    const peer = h.make(); const peerRead = h.track(peer.api.list()); await h.gets()[1].started;
    const fresh = h.track(h.api.list({}, undefined, { fresh: true })); await h.gets()[2].started;
    assert.equal(h.gets()[0].options.signal.aborted, true); assert.equal(h.gets()[1].options.signal.aborted, false);
    h.gets()[0].release(); await assert.rejects(before, { name: 'AbortError' });
    h.gets()[1].release(); await peerRead; h.gets()[2].release(); await fresh;
  });
});

test('Session retirement fences old JSON without deleting a new-session shared read', options, async () => {
  await withReads(async h => {
    const before = h.track(h.api.list()); await h.gets()[0].started;
    h.client.retireSession('next-controlled-csrf');
    const after = h.track(h.api.list({}, undefined, { fresh: true })); await h.gets()[1].started;
    h.gets()[0].release(); await assert.rejects(before, { name: 'AbortError' });
    const shared = h.track(h.api.list()); assert.equal(h.gets().length, 2);
    h.gets()[1].release(); assert.equal(currentName(await after), OLD); assert.equal(currentName(await shared), OLD);
  });
});
