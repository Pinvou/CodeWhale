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
  assert.deepEqual(await client.managedTokens(), { items: [] });
  assert.deepEqual(sent, { area: 'token', resource: '/v1/token-accounts', method: 'GET', body: undefined, idempotencyKey: undefined });
});

test('localized callback copy crosses only the login command boundary', async () => {
  const copy = { successTitle: 'Authorization received', successMessage: 'Return to Pinvou.',
    errorTitle: 'Callback not accepted', errorMessage: 'Retry from Pinvou.' };
  let sent;
  const client = new AccountPlatformDesktopClient({ login: async value => { sent = value; return { authenticated: true }; } });
  assert.deepEqual(await client.login(copy), { authenticated: true });
  assert.deepEqual(sent, copy);
  assert.equal(Object.keys(sent).length, 4);
});
