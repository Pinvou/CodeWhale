import test from 'node:test';
import assert from 'node:assert/strict';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { safePNGImage } from '../src/features/account-platform/bff-client.js';

const id = '00000000-0000-4000-8000-000000000001';
const png = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg==';
const expiry = '2099-01-01T00:00:00Z';
const reference = { password_handle:'fixture-handle', password_policy_version:'v1', expires_at:expiry };
const recovery = { registration_operation_id:id, registration_query_token_expires_at:expiry, recovery_persisted:true };
const challenge = { captcha_id: id, background_image: png, piece_image: png, width: 280, height: 120, piece_width: 44, piece_height: 44, piece_y: 30, expires_at: expiry };

test('mobile transport preserves the finite contract and omits unknown response fields', async () => {
  const calls = [];
  const client = new AccountPlatformDesktopClient({
    createSliderCaptcha: async input => { calls.push(input); return { ...challenge, unexpected: 'discard-me' }; },
    precheckMobileRegistration: async input => { calls.push(input); return { username: 'PublicUser', available: true, ...reference, token: 'discard-me' }; },
    sendSms: async input => { calls.push(input); return { challenge_id: id, expires_at: expiry, retry_after_seconds: 60, ...recovery }; },
    registerMobile: async input => { calls.push(input); return { registration_operation_id:id, user_id: id, username: 'PublicUser', status: 'PROVISIONING', password_preserved: false, unexpected: 'discard-me' }; },
    loginSms: async input => { calls.push(input); return { enabled: true, configured: true, authenticated: true, errorCode: null }; },
  });
  assert.deepEqual(await client.createSliderCaptcha('13800000000', 'REGISTER'), challenge);
  assert.deepEqual(await client.precheckMobileRegistration('PublicUser', '13800000000', 'PublicPassword123'), { username: 'PublicUser', available: true, ...reference });
  assert.deepEqual(await client.sendSms('PublicUser', '13800000000', reference, 'REGISTER', id, 20), { challenge_id: id, expires_at: expiry, retry_after_seconds: 60, ...recovery });
  assert.deepEqual(await client.registerMobile('PublicUser', '13800000000', id, '123456', reference, recovery), { registration_operation_id:id, user_id: id, username: 'PublicUser', status: 'PROVISIONING', password_preserved: false });
  assert.equal((await client.loginSms('13800000000', id, '123456')).authenticated, true);
  assert.deepEqual(calls, [
    { mobile: '13800000000', purpose: 'REGISTER' },
    { username: 'PublicUser', mobile: '13800000000', password: 'PublicPassword123' },
    { username: 'PublicUser', mobile: '13800000000', password_handle:reference.password_handle,password_policy_version:reference.password_policy_version, purpose: 'REGISTER', captcha_id: id, captcha_offset: 20 },
    { username: 'PublicUser', mobile: '13800000000', challenge_id: id, code: '123456',password_handle:reference.password_handle,password_policy_version:reference.password_policy_version,registration_operation_id:id },
    { mobile: '13800000000', challenge_id: id, code: '123456' },
  ]);
});

test('mobile errors expose only exact known codes and bounded provider retry facts', async () => {
  for (const [raw, code, seconds] of [
    ['mobile_auth_failed:SMS_RATE_LIMITED:86400', 'SMS_RATE_LIMITED', 86400],
    ['mobile_auth_failed:SMS_CODE_INVALID', 'SMS_CODE_INVALID', undefined],
    ['mobile_auth_failed:INVALID_REQUEST', 'INVALID_REQUEST', undefined],
    ['mobile_auth_failed:SMS_RATE_LIMITED:86401', 'mobile_auth_failed', undefined],
    ['mobile_auth_failed:SMS_UNAVAILABLE:60', 'mobile_auth_failed', undefined],
    ['private body SMS_RATE_LIMITED:60', 'account_platform_request_failed', undefined],
    ['mobile_auth_failed:UNKNOWN_PROVIDER_ERROR:60', 'mobile_auth_failed', undefined],
  ]) {
    const client = new AccountPlatformDesktopClient({ sendSms: async () => { throw raw; } });
    await assert.rejects(client.sendSms('', '13800000000', '', 'LOGIN', id, 20), error => {
      assert.equal(error.code, code); assert.equal(error.message, code);
      assert.equal(error.retryAfterSeconds, seconds); return true;
    });
  }
});

test('mobile malformed DTOs fail closed before rendering images or accepting a challenge', async () => {
  for (const value of [null, [], { ...challenge, background_image: 'https://untrusted.invalid/image' },
    { ...challenge, width: 4097 }, { ...challenge, piece_y: 100 }, { ...challenge, expires_at: 'not-a-date' }]) {
    const client = new AccountPlatformDesktopClient({ createSliderCaptcha: async () => value });
    await assert.rejects(client.createSliderCaptcha('13800000000', 'LOGIN'), { code: 'account_platform_request_failed' });
  }
  for (const retry of [0, -1, 1.5, 86401, '60']) {
    const client = new AccountPlatformDesktopClient({ sendSms: async () => ({ challenge_id: id, expires_at: expiry, retry_after_seconds: retry }) });
    await assert.rejects(client.sendSms('', '13800000000', '', 'LOGIN', id, 20), { code: 'account_platform_request_failed' });
  }
  const guest = new AccountPlatformDesktopClient({ loginSms: async () => ({ enabled: true, configured: true, authenticated: false, errorCode: null }) });
  await assert.rejects(guest.loginSms('13800000000', id, '123456'), { code: 'authentication_required', status: 401 });
});

test('PNG image validation refuses remote, wrong type, huge, or structurally invalid images', () => {
  assert.equal(safePNGImage(png), png);
  for (const value of [null, 'https://untrusted.invalid/image.png', 'data:image/jpeg;base64,AAAA', 'data:image/png;base64,AAAA', png + 'A'.repeat(524288)]) assert.equal(safePNGImage(value), '');
  const bytes = Uint8Array.from(atob(png.split(',')[1]), character => character.charCodeAt(0));
  bytes.set([0, 0, 16, 1], 16);
  assert.equal(safePNGImage('data:image/png;base64,' + btoa(String.fromCharCode(...bytes))), '');
});

test('new personal read routes preserve decimal-string facts without introducing authority fields', async () => {
  const calls = [];
  const client = new AccountPlatformDesktopClient({ request: async value => { calls.push(value); return { status: 200, body: { items: [{ amount_fen: '9007199254740993' }] } }; } });
  assert.equal((await client.rechargeOrders()).items[0].amount_fen, '9007199254740993');
  assert.equal((await client.entitlementPackages()).items[0].amount_fen, '9007199254740993');
  assert.deepEqual(calls.map(value => [value.area, value.resource, value.method]), [
    ['token', '/v1/recharge/orders', 'GET'], ['token', '/v1/me/entitlement-packages', 'GET'],
  ]);
  assert.ok(calls.every(value => value.body === undefined));
});

test('registration requires an explicit preserved-password fact and omits unknown fields', async () => {
  const dto = { registration_operation_id:id, user_id: id, username: 'PublicUser', status: 'PROVISIONING' };
  for (const password_preserved of [undefined, null, 'true', 'false', 0, 1, {}, []]) {
    const client = new AccountPlatformDesktopClient({ registerMobile: async () => ({ ...dto, password_preserved }) });
    await assert.rejects(client.registerMobile('PublicUser', '13800000000', id, '123456', reference, recovery), { code: 'account_platform_request_failed' });
  }
  for (const password_preserved of [false, true]) {
    const client = new AccountPlatformDesktopClient({ registerMobile: async () => ({ ...dto, password_preserved, unexpected: 'discard-me' }) });
    assert.deepEqual(await client.registerMobile('PublicUser', '13800000000', id, '123456', reference, recovery), { ...dto, password_preserved });
  }
});

test('registration progress rejects unknown states, wrong operations and every nonboolean preservation fact', async () => {
  const dto = { registration_operation_id:id, status:'IDENTITY_CREATING', password_preserved:true };
  for (const bad of [{ status:'PREPARED' },{ registration_operation_id:'nil' },{ password_preserved:undefined },{ password_preserved:'true' },{ next_retry_at:'bad' }]) {
    const client = new AccountPlatformDesktopClient({ registrationStatus:async () => ({ ...dto,...bad }) });
    await assert.rejects(client.registrationStatus(),{ code:'account_platform_request_failed' });
  }
  const client = new AccountPlatformDesktopClient({ registrationStatus:async () => ({ ...dto,token:'discard' }) });
  assert.deepEqual(await client.registrationStatus(),dto);
});
test('query credentials never cross the native reply even when only process recovery is available', async () => {
  const dto = { challenge_id:id,expires_at:expiry,retry_after_seconds:60,...recovery,recovery_persisted:false };
  let client = new AccountPlatformDesktopClient({ sendSms:async () => dto });
  assert.equal((await client.sendSms('PublicUser','13800000000',reference,'REGISTER',id,20)).recovery_persisted,false);
  client = new AccountPlatformDesktopClient({ sendSms:async () => ({ ...dto,registration_query_token:'untrusted-proof' }) });
  await assert.rejects(client.sendSms('PublicUser','13800000000',reference,'REGISTER',id,20),{ code:'account_platform_request_failed' });
});
