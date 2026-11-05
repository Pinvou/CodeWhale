import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { AccountPlatformDesktopClient } from '../../../pinvou3-app/src/features/account-platform/desktop-client.js';
import { normalizeAccountAuthError } from '../../../pinvou3-app/src/features/account-platform/auth-error.js';
import { accountPlatformCopy } from '../../../pinvou3-app/src/shared/account-platform-i18n.js';

const proof = Object.freeze({ operation_id: '11111111-1111-4111-8111-111111111111', mobile: '13800138000', purpose: 'REGISTER', captcha_id: '22222222-2222-4222-8222-222222222222', captcha_offset: 128 });
test('desktop transport preserves the same password preparation operation on explicit unknown-ACK retry', async () => {
  const calls=[];
  const transport={ async prepareRegistrationPassword(input) { calls.push(input); if (calls.length===1) throw 'mobile_auth_failed: PASSWORD_PREPARATION_UNAVAILABLE'; return {password_handle:'PUBLIC_SYNTHETIC_HANDLE',password_policy_version:'user-v1',authorization_token:'PUBLIC_SYNTHETIC_GRANT'}; } };
  const client=new AccountPlatformDesktopClient(transport);
  await assert.rejects(client.prepareRegistrationPassword('SyntheticPassword9',proof), {code:'PASSWORD_PREPARATION_UNAVAILABLE'});
  assert.equal(calls.length,1,'no implicit repeat or new operation');
  const reference=await client.prepareRegistrationPassword('SyntheticPassword9',proof);
  assert.deepEqual(calls[0],calls[1]);assert.equal(reference.password_handle,'PUBLIC_SYNTHETIC_HANDLE');
  assert.equal(calls[0].operation_id,proof.operation_id);
});
test('SMS and final reset carry only the admitted reference and capability across Account boundary', async () => {
  const calls=[];const client=new AccountPlatformDesktopClient({async sendSms(input){calls.push(['sms',input]);return {};},async resetPassword(input){calls.push(['reset',input]);return {};}});
  const ref={password_handle:'PUBLIC_SYNTHETIC_HANDLE',password_policy_version:'user-v1',authorization_token:'PUBLIC_SYNTHETIC_GRANT'};
  await client.sendSms('synthetic',proof.mobile,ref,'REGISTER',proof.captcha_id,proof.captcha_offset);
  await client.resetPassword(proof.mobile,'PUBLIC_SYNTHETIC_CHALLENGE','000000',ref);
  assert.equal(calls[0][1].password_preparation_token,ref.authorization_token);
  assert.deepEqual(calls[1][1],{mobile:proof.mobile,challenge_id:'PUBLIC_SYNTHETIC_CHALLENGE',code:'000000',password_handle:ref.password_handle,password_policy_version:ref.password_policy_version});
  for(const [,body] of calls)assert.ok(!Object.hasOwn(body,'password'));
});
test('existing global 429 hint is bounded and preparation errors have three-language presentation', () => {
  assert.equal(normalizeAccountAuthError('mobile_auth_failed: RATE_LIMITED:999999').retryAfterSeconds,86400);
  assert.equal(normalizeAccountAuthError('mobile_auth_failed: RATE_LIMITED:0').retryAfterSeconds,0);
  for(const language of ['zh','en','ja']) {
    assert.equal(typeof accountPlatformCopy[language].authRequestRateLimited,'string');
    assert.ok(accountPlatformCopy[language].authRequestRateLimited.length>5);
    assert.ok(accountPlatformCopy[language].captchaInvalid.length>5);
  }
});
test('public preparation schema accepts only challenge facts and both password purposes', async () => {
  const spec=JSON.parse(await readFile(new URL('../../contracts/openapi/registration-signup.json',import.meta.url),'utf8'));
  const schema=spec.paths['/v1/auth/password-preparation-authorizations'].post.requestBody.content['application/json'].schema;
  assert.equal(schema.additionalProperties,false);
  assert.deepEqual(new Set(Object.keys(schema.properties)),new Set(['operation_id','mobile','purpose','captcha_id','captcha_offset']));
  assert.deepEqual(schema.properties.purpose.enum,['REGISTER','PASSWORD_RESET']);
  assert.ok(!('password' in schema.properties));
});
