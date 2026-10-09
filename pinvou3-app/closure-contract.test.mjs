import assert from 'node:assert/strict';
import test from 'node:test';
import { AccountPlatformBffClient } from './src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from './src/features/account-platform/desktop-client.js';
import { requireAccountClosure } from './src/features/account-platform/account-closure-contract.js';
import { queryAccountClosure, submitAccountClosure, accountClosureResumeAction } from './src/features/account-platform/account-closure-flow.js';
import { accountClosureCopy, financeCopy } from './src/shared/i18n.js';
const receipt = { closure_request_id: '11111111-1111-4111-8111-111111111111', status: 'IN_PROGRESS', stage: 'CLOSURE_REQUESTED', blockers: [] };
test('BFF closure uses real same-origin CSRF and original idempotency header on retries', async () => {
  const previous = globalThis.fetch; const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, ...options }); return new Response(JSON.stringify(receipt), { status: options.method === 'POST' ? 202 : 200, headers: { 'Content-Type': 'application/json' } }); };
  try {
    const client = new AccountPlatformBffClient(); client.csrfToken = 'controlled-session-csrf';
    await client.accountClosure(); await client.createAccountClosure('CLOSE_MY_ACCOUNT', 'closure-original-key'); await client.createAccountClosure('CLOSE_MY_ACCOUNT', 'closure-original-key');
    assert.equal(calls[0].url, '/api/account/v1/me/account-closure-request'); assert.equal(calls[0].method, 'GET');
    for (const call of calls.slice(1)) { assert.equal(call.url, '/api/account/v1/me/account-closure-requests'); assert.equal(call.method, 'POST'); assert.equal(call.headers['Idempotency-Key'], 'closure-original-key'); assert.equal(call.headers['X-CSRF-Token'], 'controlled-session-csrf'); assert.deepEqual(JSON.parse(call.body), { confirmation: 'CLOSE_MY_ACCOUNT' }); }
  } finally { globalThis.fetch = previous; }
});
test('Native client sends exact area/resource/body/key through inherited transport', async () => {
  const calls = []; const client = new AccountPlatformDesktopClient({ request: async input => { calls.push(input); return { status: input.method === 'POST' ? 202 : 200, body: receipt }; } });
  await client.accountClosure(); await client.createAccountClosure('CLOSE_MY_ACCOUNT', 'closure-original-key');
  assert.deepEqual(calls.map(({ area, resource, method, body, idempotencyKey }) => ({ area, resource, method, body, idempotencyKey })), [
    { area: 'account', resource: '/v1/me/account-closure-request', method: 'GET', body: undefined, idempotencyKey: undefined },
    { area: 'account', resource: '/v1/me/account-closure-requests', method: 'POST', body: { confirmation: 'CLOSE_MY_ACCOUNT' }, idempotencyKey: 'closure-original-key' },
  ]);
});
test('Only authoritative absence enables first request; failed reads remain unavailable', async () => {
  for (const status of [401, 403, 500]) assert.equal((await queryAccountClosure({ accountClosure: async () => { throw Object.assign(new Error(), { status }); } })).status, 'error');
  assert.deepEqual(await queryAccountClosure({ accountClosure: async () => { throw Object.assign(new Error(), { status: 404 }); } }), { status: 'ready', closure: null });
  assert.equal(accountClosureResumeAction({ requestedClosure: 'confirm', queryStatus: 'error', closure: null }).clearMarker, false);
});
test('Closure confirmation and recent-auth round trip use the established authority contract', async () => {
  let writes = 0; let returnTo;
  const client = { createAccountClosure: async () => { writes++; throw Object.assign(new Error(), { code: 'RECENT_AUTHENTICATION_REQUIRED' }); }, reauthenticate: path => { returnTo = path; } };
  assert.equal((await submitAccountClosure({ client, confirmation: 'wrong' })).status, 'confirmation-mismatch'); assert.equal(writes, 0);
  assert.equal((await submitAccountClosure({ client, confirmation: 'CLOSE_MY_ACCOUNT', key: 'closure-original-key', returnTo: '/?account_closure=confirm' })).status, 'reauthenticating'); assert.equal(returnTo, '/?account_closure=confirm');
});
test('Incomplete ACK keeps result unknown rather than permitting a new invocation', () => {
  assert.equal(requireAccountClosure(receipt), receipt);
  for (const bad of [null, {}, { ...receipt, closure_request_id: '00000000-0000-0000-0000-000000000000' }, { ...receipt, blockers: [null] }, { ...receipt, stage: 'future' }]) assert.throws(() => requireAccountClosure(bad), error => error.code === 'COMMAND_RESULT_UNKNOWN');
});
test('Three dictionaries cover actual personal/repair labels without language fallback', () => {
  for (const language of ['zh', 'en', 'ja']) for (const key of ['title','lead','before','start','confirmPrompt','confirmMismatch','queryLoading','queryFailed','queryRetry','sla','noBlockers']) assert.equal(typeof accountClosureCopy[language][key], 'string');
  for (const language of ['zh', 'en', 'ja']) for (const key of ['reason','retry','refresh','unknown','permissions','unavailable']) assert.equal(typeof financeCopy[language][key], 'string');
});
