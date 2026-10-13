import test from 'node:test';
import assert from 'node:assert/strict';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { createDesktopReauthenticationCoordinator } from '../src/features/account-platform/desktop-reauthentication.js';
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
test('desktop recent-auth without an installed local coordinator fails closed', async () => {
  const { client, calls } = fixture(403, { code: 'RECENT_AUTHENTICATION_REQUIRED' });
  await assert.rejects(submitAccountClosure({ client, confirmation: 'CLOSE_MY_ACCOUNT', key, returnTo: '/?account_closure=confirm' }), error => error.code === 'REAUTHENTICATION_UNAVAILABLE');
  assert.equal(calls.length, 1, 'there is no Portal fallback or automatic second closure command');
  assert.equal(calls[0].idempotencyKey, key);
});
test('ordinary default-locale Portal opening remains available and reports handoff failure', async () => {
  const { client, calls } = fixture();
  await client.openPortal();
  assert.deepEqual(calls, [{ language: 'zh-Hans' }]);
  client.transport.openPortal = async () => { throw new Error('controlled handoff failure'); };
  await assert.rejects(client.openPortal(), error => error.code === 'REQUEST_FAILED');
});

const actor = '00000000-0000-4000-8000-000000000001';
const profile = { user_id: actor, status: 'ACTIVE', personal_permissions: [], platform_permissions: [] };
for (const [language, tag] of [['zh', 'zh-Hans'], ['en', 'en'], ['ja', 'ja']]) {
  test(`ordinary desktop Portal opening preserves ${language}`, async () => {
    const { client, calls } = fixture();
    await client.openPortal(language);
    assert.deepEqual(calls, [{ language: tag }]);
  });
  test(`desktop native re-login retains ${language} and the original command key until explicit retry`, async () => {
    const calls = [];
    let recent = false, ticket, prompt;
    const prompted = new Promise(resolve => { prompt = resolve; });
    const client = new AccountPlatformDesktopClient({
      request: async input => {
        calls.push(input);
        if (input.resource === '/v1/account/bootstrap') return { status: 200, body: profile };
        return recent ? { status: 202, body: { stage: 'CLOSURE_REQUESTED' } } : { status: 403, body: { code: 'RECENT_AUTHENTICATION_REQUIRED' } };
      },
      login: async input => { assert.deepEqual(input, { account: 'controlled-original-user', password: 'controlled-test-input' }); recent = true; },
      openPortal: async () => { assert.fail('native re-login must not call Portal'); },
    });
    const coordinator = createDesktopReauthenticationCoordinator({ client, getActor: () => actor, onPrompt: value => { ticket = value; prompt(); }, onVerified: () => {} });
    const unbind = client.bindReauthentication(value => coordinator.request(value));
    const pending = submitAccountClosure({ client, confirmation: 'CLOSE_MY_ACCOUNT', key, returnTo: '/?account_closure=confirm', language });
    await prompted;
    assert.equal(ticket.language, language);
    assert.equal(ticket.actor, actor);
    await coordinator.authenticate(ticket, () => client.login('controlled-original-user', 'controlled-test-input'));
    coordinator.complete(ticket);
    const result = await pending;
    assert.equal(result.status, 'reauthenticated');
    assert.equal(result.closure, undefined, 'authentication is not a business receipt');
    assert.equal(calls.filter(value => value.resource === '/v1/me/account-closure-requests').length, 1);
    assert.equal(client.financeSessionEpoch, 1);
    assert.equal(JSON.stringify(ticket).includes('controlled-test-input'), false);
    const retried = await submitAccountClosure({ client, confirmation: 'CLOSE_MY_ACCOUNT', key, language });
    assert.equal(retried.status, 'created');
    const writes = calls.filter(value => value.resource === '/v1/me/account-closure-requests');
    assert.equal(writes.length, 2);
    assert.deepEqual(writes.map(value => [value.idempotencyKey, value.body]), [[key, { confirmation: 'CLOSE_MY_ACCOUNT' }], [key, { confirmation: 'CLOSE_MY_ACCOUNT' }]]);
    unbind(); coordinator.dispose();
  });
}
