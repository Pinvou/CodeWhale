import test from 'node:test';
import assert from 'node:assert/strict';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { submitAccountClosure } from '../src/features/account-platform/account-closure-flow.js';

const key = 'controlled-closure-key';
function fixture(status = 200, body = { stage: 'CLOSURE_REQUESTED' }) {
  const calls = [];
  const client = new AccountPlatformDesktopClient({ request: async input => { calls.push(input); return { status, body }; }, openPortal: async input => { calls.push(input); } });
  return { client, calls };
}
test('desktop closure read and keyed write use only the existing Account routes', async () => {
  const { client, calls } = fixture();
  await client.accountClosure(); await client.createAccountClosure('CLOSE_MY_ACCOUNT', key);
  assert.deepEqual(calls.map(value => [value.area, value.resource, value.method, value.idempotencyKey, value.body]), [
    ['account', '/v1/me/account-closure-request', 'GET', undefined, undefined],
    ['account', '/v1/me/account-closure-requests', 'POST', key, { confirmation: 'CLOSE_MY_ACCOUNT' }],
  ]);
});
test('desktop preserves authoritative 404 read and 403 mutation errors', async () => {
  await assert.rejects(fixture(404, { code: 'NOT_FOUND' }).client.accountClosure(), error => error.status === 404 && error.code === 'NOT_FOUND');
  await assert.rejects(fixture(403, { code: 'FORBIDDEN' }).client.createAccountClosure('CLOSE_MY_ACCOUNT', key), error => error.status === 403 && error.code === 'FORBIDDEN');
});
test('desktop recent-auth rejection awaits the existing default-locale Portal handoff', async () => {
  const { client, calls } = fixture(403, { code: 'RECENT_AUTHENTICATION_REQUIRED' });
  const result = await submitAccountClosure({ client, confirmation: 'CLOSE_MY_ACCOUNT', key, returnTo: '/?account_closure=confirm' });
  assert.equal(result.status, 'reauthenticating'); assert.deepEqual(calls[1], { language: 'zh-Hans' });
});
test('Portal handoff failure is observable and never becomes a closure receipt', async () => {
  const { client } = fixture(403, { code: 'RECENT_AUTHENTICATION_REQUIRED' });
  client.transport.openPortal = async () => { throw new Error('controlled handoff failure'); };
  await assert.rejects(submitAccountClosure({ client, confirmation: 'CLOSE_MY_ACCOUNT', key }), error => error.code === 'REQUEST_FAILED');
});
