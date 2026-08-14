import assert from 'node:assert/strict';
import test from 'node:test';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from '../src/features/account-platform/context-guard.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';

test('context guard invalidates and aborts stale organization loads', () => {
  const guard = createContextRequestGuard();
  const first = guard.begin();
  const second = guard.begin();
  assert.equal(first.signal.aborted, true);
  assert.equal(guard.isCurrent(first.generation), false);
  assert.equal(guard.isCurrent(second.generation), true);
});

test('recharge polling backs off from two to five seconds with bounded jitter', () => {
  assert.equal(rechargePollDelay(0, () => 0.5), 2000);
  assert.equal(rechargePollDelay(3, () => 0.5), 3500);
  assert.equal(rechargePollDelay(99, () => 0.5), 5000);
  assert.ok(rechargePollDelay(0, () => 0) >= 1800);
  assert.ok(rechargePollDelay(99, () => 1) <= 5500);
});

test('organization context never accepts a personal Token candidate', () => {
  const items = [{ token_account_id: 'personal', owner_type: 'USER' }, { token_account_id: 'organization', owner_type: 'ORG' }];
  assert.deepEqual(eligibleForContext(items, 'organization-id').map(item => item.token_account_id), ['organization']);
  assert.deepEqual(eligibleForContext(items, '').map(item => item.token_account_id), ['personal']);
});

test('BFF client keeps CSRF on mutations and never persists OIDC tokens', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    if (String(url).endsWith('/api/session')) return new Response(JSON.stringify({ authenticated: true, csrf_token: 'c'.repeat(32) }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('https://portal.example');
    await client.session();
    await client.createOrganization('Example');
    assert.equal(calls[1].options.headers['X-CSRF-Token'], 'c'.repeat(32));
    assert.equal(calls[1].options.credentials, 'same-origin');
    assert.equal('accessToken' in client, false);
    assert.equal('refreshToken' in client, false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('BFF client restores the recent order without browser persistence', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ order: null }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    assert.deepEqual(await client.recentRechargeOrder(), { order: null });
    assert.equal(calls[0].url, '/portal/api/token/v1/recharge/orders/recent');
    assert.equal(calls[0].options.cache, 'no-store');
    assert.equal('recentOrder' in client, false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('settlement status is queried once and never resends a model request', async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ display_status: 'SETTLEMENT_PENDING' }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    const status = await client.usageRequest('usage-id');
    assert.equal(status.display_status, 'SETTLEMENT_PENDING');
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, '/portal/api/token/v1/usage-requests/usage-id');
    assert.equal(calls[0].options.method, 'GET');
    assert.equal(calls[0].url.includes('/chat/completions'), false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('desktop client keeps OAuth tokens behind the account-platform Bridge', async () => {
  const calls = [];
  const transport = {
    session: async () => ({ enabled: true, configured: true, authenticated: true }),
    login: async (username, password) => {
      calls.push({ username, password });
      return { authenticated: true };
    },
    logout: async () => ({ authenticated: false }),
    request: async request => {
      calls.push(request);
      return { status: 200, body: { items: [] } };
    },
  };
  const client = new AccountPlatformDesktopClient(transport);
  assert.equal((await client.session()).authenticated, true);
  await client.login('user@example.test', 'one-time-password');
  await client.createRecharge('product-10', 'ALIPAY', 'recharge-id');
  assert.deepEqual(calls[0], { username: 'user@example.test', password: 'one-time-password' });
  assert.deepEqual(calls[1], {
    area: 'token',
    resource: '/v1/recharge/orders',
    method: 'POST',
    body: { product_id: 'product-10', channel: 'ALIPAY' },
    idempotencyKey: 'recharge-id',
  });
  assert.equal('accessToken' in client, false);
  assert.equal('refreshToken' in client, false);
});

test('desktop client surfaces backend error codes without exposing auth state', async () => {
  const client = new AccountPlatformDesktopClient({
    request: async () => ({ status: 403, body: { code: 'permission_denied', message: 'denied' } }),
  });
  await assert.rejects(client.organizations(), error => {
    assert.equal(error.code, 'permission_denied');
    assert.equal(error.status, 403);
    return true;
  });
});
