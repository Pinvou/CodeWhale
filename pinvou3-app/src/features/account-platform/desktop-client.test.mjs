import assert from 'node:assert/strict';
import { test } from 'node:test';
import { AccountPlatformDesktopClient } from './desktop-client.js';

test('native authentication failure is a typed 401 without provider detail', async () => {
  for (const value of ['authentication_required: private provider body', 'authentication_required: credential_cleanup_failed']) {
    const client = new AccountPlatformDesktopClient({ session: async () => { throw value; } });
    await assert.rejects(client.session(), error => error instanceof Error
      && error.code === 'authentication_required' && error.status === 401
      && error.message === 'authentication_required');
  }
});

test('native transport and credential failures preserve their temporary classification', async () => {
  for (const code of ['oidc_token_endpoint_unavailable', 'credential_store_failed', 'oidc_token_exchange_failed']) {
    const client = new AccountPlatformDesktopClient({ login: async () => { throw `${code}: private detail`; } });
    await assert.rejects(client.login(), error => error.code === code && error.status === undefined && error.message === code);
  }
});

test('unknown rejected values never expose their body to the WebView', async () => {
  for (const value of [new Error('private URL'), { message: 'private body' }, 'invalid code: private data']) {
    const client = new AccountPlatformDesktopClient({ session: async () => { throw value; } });
    await assert.rejects(client.session(), { message: 'account_platform_request_failed', code: 'account_platform_request_failed' });
  }
});

test('abort hides a late native result and a late native rejection', async () => {
  for (const reject of [false, true]) {
    const controller = new AbortController();
    const client = new AccountPlatformDesktopClient({ session: async () => {
      controller.abort();
      if (reject) throw 'authentication_required';
      return { authenticated: true };
    } });
    await assert.rejects(client.session(controller.signal), { name: 'AbortError' });
  }
});

test('allowlisted business requests transport data without OAuth fields', async () => {
  let sent;
  const client = new AccountPlatformDesktopClient({ request: async value => { sent = value; return { status: 200, body: { items: [] } }; } });
  assert.deepEqual(await client.usageHistory('next/page'), { items: [] });
  assert.deepEqual(sent, { area: 'token', resource: '/v1/me/usage-requests?limit=20&cursor=next%2Fpage', method: 'GET', body: undefined, idempotencyKey: undefined });
});

test('credentials cross only the native login boundary and status is validated', async () => {
  let sent;
  const status = { enabled: true, configured: true, authenticated: true, errorCode: null };
  const client = new AccountPlatformDesktopClient({ login: async value => { sent = value; return status; } });
  assert.deepEqual(await client.login('desktop-user', 'test-only-password'), status);
  assert.deepEqual(sent, { account: 'desktop-user', password: 'test-only-password' });
  assert.deepEqual(Object.keys(status).sort(), ['authenticated', 'configured', 'enabled', 'errorCode']);
});

test('malformed or inconsistent native sessions cannot become authenticated', async () => {
  for (const value of [null, [], {}, { authenticated: true }, { enabled: false, configured: true, authenticated: true }, { enabled: true, configured: true, authenticated: 'true' }]) {
    const client = new AccountPlatformDesktopClient({ session: async () => value, login: async () => value });
    await assert.rejects(client.session(), { code: 'account_platform_request_failed' });
    await assert.rejects(client.login('user', 'test-password'), { code: 'account_platform_request_failed' });
  }
});

test('login cannot report success with a guest status', async () => {
  const client = new AccountPlatformDesktopClient({ login: async () => ({ enabled: true, configured: true, authenticated: false, errorCode: 'oidc_token_endpoint_unavailable' }) });
  await assert.rejects(client.login('user', 'test-password'), { code: 'oidc_token_endpoint_unavailable' });
});

test('logout cannot report success without a validated guest status', async () => {
  for (const value of [null, {}, { enabled: true, configured: true, authenticated: true, errorCode: null }]) {
    const client = new AccountPlatformDesktopClient({ logout: async () => value });
    await assert.rejects(client.logout(), { code: 'account_platform_request_failed' });
  }
  const guest = { enabled: true, configured: true, authenticated: false, errorCode: null };
  assert.deepEqual(await new AccountPlatformDesktopClient({ logout: async () => guest }).logout(), guest);
});

test('business errors hide provider detail and malformed success fails closed', async () => {
  for (const response of [{ status: 403, body: { code: 'ACCESS_DENIED', message: 'private provider body' } }, { status: 200, body: null }, null]) {
    const client = new AccountPlatformDesktopClient({ request: async () => response });
    await assert.rejects(client.balance(), error => !error.message.includes('private') && Number.isInteger(error.status));
  }
});

test('existing user-managed Token operations preserve their native contracts and cancellation', async () => {
  const sent = []; const client = new AccountPlatformDesktopClient({ request: async value => { sent.push(value); return { status: 200, body: { items: [] } }; } });
  await client.managedTokens(); await client.eligibleTokens('model / a'); await client.usageRequest('usage-id');
  const owner = { owner_type: 'USER', owner_id: 'user-id', name: 'owned' }; await client.createTokenAccount(owner);
  await client.credentials('account-id'); const credential = { provider: 'new-api', base_url: 'https://example.invalid', secret: 'test-only-secret' }; await client.rotateCredential('account-id', credential);
  assert.deepEqual(sent.map(x => [x.area, x.resource, x.method, x.body]), [
    ['token', '/v1/token-accounts', 'GET', undefined], ['token', '/v1/token-accounts/eligible?model=model%20%2F%20a', 'GET', undefined],
    ['token', '/v1/usage-requests/usage-id', 'GET', undefined], ['token', '/v1/token-accounts', 'POST', owner],
    ['token', '/v1/token-accounts/account-id/credentials', 'GET', undefined], ['token', '/v1/token-accounts/account-id/credentials', 'POST', credential],
  ]);
  const controller = new AbortController(); controller.abort();
  for (const invoke of [() => client.managedTokens(controller.signal), () => client.eligibleTokens('model', controller.signal), () => client.usageRequest('usage-id', controller.signal), () => client.credentials('account-id', controller.signal)]) await assert.rejects(invoke(), { name: 'AbortError' });
  assert.equal(sent.length, 6);
});
