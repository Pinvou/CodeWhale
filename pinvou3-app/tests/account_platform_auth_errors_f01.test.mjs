import test from 'node:test';
import assert from 'node:assert/strict';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { normalizeAccountAuthError, retryAfterSeconds } from '../src/features/account-platform/auth-error.js';
import { localizedAccountPlatformError } from '../src/features/account-platform/error-localization.js';
import { accountPlatformCopy } from '../src/shared/i18n.js';

test('native password and SMS login rejections map to specific messages in all three languages', async () => {
  for (const code of ['oidc_credentials_invalid', 'mobile_auth_code_invalid']) {
    // Tauri serializes Result<_, String> rejections as strings, not JS Errors.
    // eslint-disable-next-line prefer-promise-reject-errors
    const client = new AccountPlatformDesktopClient({ login: () => Promise.reject(`${code}: private provider text`) });
    await assert.rejects(client.login('account', 'password'), error => {
      assert.equal(error.code, code);
      assert.equal(error.message, code);
      for (const language of ['zh', 'en', 'ja']) {
        const copy = accountPlatformCopy[language];
        assert.equal(localizedAccountPlatformError(copy, error), copy.errors[code]);
        assert.notEqual(copy.errors[code], copy.requestFailed);
      }
      return true;
    });
  }
});

test('native SMS rate limits retain bounded cooldown, including wrapped Tauri errors', async () => {
  for (const reason of ['mobile_auth_failed: SMS_RATE_LIMITED:120', new Error('mobile_auth_failed: SMS_RATE_LIMITED:120')]) {
    const client = new AccountPlatformDesktopClient({ sendSms: async () => { throw reason; } });
    await assert.rejects(client.sendSms('', '', null, 'LOGIN', '', 0), error => {
      assert.equal(error.code, 'SMS_RATE_LIMITED');
      assert.equal(error.retryAfterSeconds, 120);
      return true;
    });
  }
  assert.equal(normalizeAccountAuthError('mobile_auth_failed: SMS_RATE_LIMITED:999999').retryAfterSeconds, 86_400);
  assert.equal(normalizeAccountAuthError('mobile_auth_failed: SMS_RATE_LIMITED:12garbage').retryAfterSeconds, 0);
  assert.equal(normalizeAccountAuthError('mobile_auth_failed: SMS_UNAVAILABLE:120').retryAfterSeconds, 0);
  assert.equal(normalizeAccountAuthError({ code: 'SMS_UNAVAILABLE', message: 'mobile_auth_failed: SMS_RATE_LIMITED:120' }).code, 'SMS_UNAVAILABLE');
  assert.equal(normalizeAccountAuthError({ code: 'SMS_UNAVAILABLE', message: 'mobile_auth_failed: SMS_RATE_LIMITED:120' }).retryAfterSeconds, 0);
  const aborted = Object.assign(new Error('cancelled'), { name: 'AbortError' });
  assert.equal(normalizeAccountAuthError(aborted), aborted);
});

test('desktop HTTP error responses preserve body and header retry metadata', async () => {
  for (const response of [
    { status: 429, body: { code: 'SMS_RATE_LIMITED', details: { retry_after_seconds: 60 } } },
    { status: 429, body: { code: 'SMS_RATE_LIMITED' }, retryAfterSeconds: 60 },
  ]) {
    const client = new AccountPlatformDesktopClient({ request: async () => response });
    await assert.rejects(client.request('account', '/v1/me'), error => error.code === 'SMS_RATE_LIMITED' && error.retryAfterSeconds === 60);
  }
});

test('BFF HTTP errors retain Retry-After and body fallback without raw-error localization', async t => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  for (const bodySeconds of [undefined, 45]) {
    globalThis.fetch = async () => new Response(JSON.stringify({ code: 'SMS_RATE_LIMITED', details: { retry_after_seconds: bodySeconds } }), {
      status: 429, headers: { 'Content-Type': 'application/json', 'Retry-After': '60' },
    });
    await assert.rejects(new AccountPlatformBffClient().session(), error => error.code === 'SMS_RATE_LIMITED' && error.retryAfterSeconds === (bodySeconds || 60));
  }
});

test('cooldown parser accepts seconds and HTTP date and rejects malformed or past values', () => {
  const now = Date.parse('2026-10-08T12:00:00Z');
  assert.equal(retryAfterSeconds('Thu, 08 Oct 2026 12:01:00 GMT', now), 60);
  for (const value of ['', '-1', '1.5', '60garbage', '0', -1, NaN, Infinity, 'Thu, 08 Oct 2026 11:59:00 GMT']) {
    assert.equal(retryAfterSeconds(value, now), 0);
  }
});

test('generic native wrappers retain nested catalog codes without retaining provider text', () => {
  const error = normalizeAccountAuthError('wrapper: oidc_credentials_invalid: PRIVATE_PROVIDER_DETAIL');
  assert.equal(error.code, 'oidc_credentials_invalid');
  assert.equal(localizedAccountPlatformError(accountPlatformCopy.en, error), accountPlatformCopy.en.errors.oidc_credentials_invalid);
  assert.equal(error.message.includes('PRIVATE_PROVIDER_DETAIL'), false);
});
