import assert from 'node:assert/strict';
import test from 'node:test';
import { AccountPlatformBffClient } from './bff-client.js';

async function withFetch(implementation, check) {
  const previous = globalThis.fetch;
  globalThis.fetch = implementation;
  try { await check(); } finally { globalThis.fetch = previous; }
}

const json = (value, status = 200) => new Response(JSON.stringify(value), { status });

test('malformed session responses cannot become authentication or retain stale CSRF', async () => {
  for (const value of [null, [], {}, { authenticated: 'true' }, { authenticated: true }, { authenticated: true, csrf_token: 123 }]) {
    await withFetch(async () => json(value), async () => {
      const client = new AccountPlatformBffClient();
      client.csrfToken = 'stale-fixture-token';
      await assert.rejects(client.session(), { code: 'REQUEST_FAILED', message: 'REQUEST_FAILED' });
      assert.equal(client.csrfToken, '');
    });
  }
});

test('authenticated sessions supply CSRF to mutations and guest sessions clear it', async () => {
  const calls = [];
  await withFetch(async (url, options) => {
    calls.push({ url, options });
    if (url === '/api/session') return json({ authenticated: true, csrf_token: 'public-fixture-csrf' });
    return json({ user_id: 'public-fixture-user' });
  }, async () => {
    const client = new AccountPlatformBffClient();
    await client.session();
    assert.deepEqual(await client.bootstrap(), { user_id: 'public-fixture-user' });
    assert.equal(calls[1].options.headers['X-CSRF-Token'], 'public-fixture-csrf');
    assert.equal(calls[1].options.credentials, 'same-origin');
  });
  for (const response of [json({ authenticated: false }), json({ authenticated: false }, 401)]) {
    await withFetch(async () => response, async () => {
      const client = new AccountPlatformBffClient();
      client.csrfToken = 'stale-fixture-token';
      assert.deepEqual(await client.session(), { authenticated: false });
      assert.equal(client.csrfToken, '');
    });
  }
});

test('invalid successful business bodies never masquerade as completed mutations', async () => {
  for (const body of ['not JSON', 'null', '[]', 'true']) {
    await withFetch(async () => new Response(body, { status: 200 }), async () => {
      await assert.rejects(new AccountPlatformBffClient().createRecharge('CNY_1', 'ALIPAY', 'public-fixture-key'),
        { code: 'REQUEST_FAILED', message: 'REQUEST_FAILED' });
    });
  }
  await withFetch(async () => new Response(null, { status: 204 }), async () => {
    assert.equal(await new AccountPlatformBffClient().request('account', '/fixture', { method: 'DELETE' }), null);
  });
});

test('HTTP errors preserve status and safe code without leaking provider bodies', async () => {
  for (const value of [{ code: 'PERMISSION_DENIED', message: 'PRIVATE_PROVIDER_BODY' }, { code: 'invalid code: PRIVATE_PROVIDER_BODY' }, null]) {
    await withFetch(async () => json(value, 403), async () => {
      await assert.rejects(new AccountPlatformBffClient().createUnit('org', 'unit'), error => {
        assert.equal(error.status, 403);
        assert.equal(error.code, value?.code === 'PERMISSION_DENIED' ? 'PERMISSION_DENIED' : 'REQUEST_FAILED');
        assert.equal(error.message, error.code);
        return true;
      });
    });
  }
  await withFetch(async () => json({}, 401), async () => {
    const client = new AccountPlatformBffClient(); client.csrfToken = 'stale-fixture-token';
    await assert.rejects(client.balance(), { status: 401 });
    assert.equal(client.csrfToken, '');
  });
});

test('body cancellation remains AbortError and failed logout retains the session token', async () => {
  await withFetch(async () => ({ ok: true, status: 200, json: async () => { throw new DOMException('fixture cancellation', 'AbortError'); } }), async () => {
    await assert.rejects(new AccountPlatformBffClient().balance(), { name: 'AbortError' });
  });
  await withFetch(async () => json({ message: 'PRIVATE_PROVIDER_BODY' }, 503), async () => {
    const client = new AccountPlatformBffClient(); client.csrfToken = 'public-fixture-csrf';
    await assert.rejects(client.logout(), { code: 'REQUEST_FAILED', status: 503, message: 'REQUEST_FAILED' });
    assert.equal(client.csrfToken, 'public-fixture-csrf');
  });
});
