import assert from 'node:assert/strict';
import test from 'node:test';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { OrganizationClient } from '../src/features/account-platform/organization-client.js';
import { parseClosedGrantsJSON } from '../src/features/account-platform/account-closed-grants-contract.js';
import { readClosureIntent, saveClosureIntent, clearClosureIntent, closureRepairIntents, closureGrantIntents } from '../src/features/account-platform/closure-intent.js';
import { accountClosureCopy, accountClosedGrantCopy } from '../src/shared/i18n.js';
import { requireAccountClosure } from '../src/features/account-platform/account-closure-contract.js';

const actor = '11111111-1111-4111-8111-111111111111';
const other = '22222222-2222-4222-8222-222222222222';
const grant = '33333333-3333-4333-8333-333333333333';
const key = 'closure-original-command-key';
for (const [name, value] of [['missing', undefined], ['null', null], ['numeric', 123], ['empty', ''], ['timezone missing', '2026-10-13T12:00:00'], ['date overflow', '2026-02-31T12:00:00Z'], ['invalid hour', '2026-10-13T24:01:00Z'], ['whitespace', ' 2026-10-13T12:00:00Z']]) {
  test(`closure receipt ${name} SLA is unconfirmed rather than a known ACK`, () => {
    const item = { closure_request_id: actor, status: 'IN_PROGRESS', stage: 'CLOSURE_REQUESTED', blockers: [], sla_expires_at: value };
    assert.throws(() => requireAccountClosure(item), error => error.code === 'COMMAND_RESULT_UNKNOWN');
  });
}
for (const value of ['1970-01-01T00:00:00Z', '2030-01-01T08:00:00.123456789+08:00', '2024-02-29t20:30:59z', '2016-12-31T23:59:60Z']) {
  test(`closure receipt keeps valid RFC3339 SLA ${value} without a deadline policy`, () => {
    const item = { closure_request_id: actor, status: 'COMPLETED', stage: 'COMPLETED', blockers: [], sla_expires_at: value };
    assert.equal(requireAccountClosure(item), item);
    assert.equal(item.sla_expires_at, value);
  });
}
const memory = () => { const values = new Map(); return { get length() { return values.size; }, key: index => [...values.keys()][index], getItem: name => values.get(name) ?? null, setItem: (name, value) => values.set(name, value), removeItem: name => values.delete(name) }; };

test('closure intent survives new client, isolates actor/resource and cannot replace uncertain facts', () => {
  const storage = memory();
  const saved = { actor, scope: 'account-closure', key, facts: { confirmation: 'CLOSE_MY_ACCOUNT' } };
  saveClosureIntent(saved, storage);
  assert.deepEqual(readClosureIntent(actor, saved.scope, storage), saved);
  assert.equal(readClosureIntent(other, saved.scope, storage), null);
  assert.throws(() => saveClosureIntent({ ...saved, key: 'closure-a-different-command-key' }, storage));
  clearClosureIntent(actor, saved.scope, 'closure-a-stale-command-key', storage);
  assert.deepEqual(readClosureIntent(actor, saved.scope, storage), saved, 'late receipt cannot delete a different invocation');
  clearClosureIntent(actor, saved.scope, key, storage);
  assert.equal(readClosureIntent(actor, saved.scope, storage), null);
});

test('repair and grant recovery keep exact targets, batch order and no authority from storage', () => {
  const storage = memory();
  const repair = { actor, scope: `account-closure-retry:${other}`, key, facts: { id: other, reason: 'Controlled repair reason' } };
  const single = { actor, scope: `closed-grant-reclaim:${other}:${grant}`, key, facts: { grant, reason: 'Controlled reclaim reason' } };
  const batch = { actor, scope: `closed-grant-reclaim-batch:${other}`, key, facts: { grants: [grant, actor], reason: 'Controlled batch reason' } };
  for (const value of [repair, single, batch]) saveClosureIntent(value, storage);
  assert.deepEqual(closureRepairIntents(actor, storage), [repair]);
  assert.deepEqual(closureGrantIntents(actor, other, storage), [single]);
  assert.deepEqual(readClosureIntent(actor, batch.scope, storage).facts.grants, [grant, actor]);
  assert.throws(() => saveClosureIntent({ ...single, facts: { ...single.facts, grant: actor } }, storage));
  assert.throws(() => saveClosureIntent({ ...batch, facts: { ...batch.facts, grants: [grant, grant] } }, storage));
});

test('a delayed original receipt cannot clear a newer invocation in the same durable resource', async () => {
  const storage = memory();
  const old = { actor, scope: `account-closure-retry:${other}`, key, facts: { id: other, reason: 'Original repair attempt' } };
  saveClosureIntent(old, storage);
  let completeOld;
  const late = new Promise(resolve => { completeOld = resolve; }).then(() => clearClosureIntent(actor, old.scope, old.key, storage));
  // Another view already received the first command's ACK, then began a new command.
  clearClosureIntent(actor, old.scope, old.key, storage);
  const current = { ...old, key: 'closure-a-new-command-after-ack', facts: { id: other, reason: 'New repair attempt after acknowledged receipt' } };
  saveClosureIntent(current, storage);
  completeOld(); await late;
  assert.deepEqual(readClosureIntent(actor, old.scope, storage), current);
});

test('corrupt, unavailable and unverified storage fails closed', () => {
  assert.throws(() => readClosureIntent(actor, 'account-closure', { getItem: () => '{broken' }));
  assert.throws(() => readClosureIntent(actor, 'account-closure', { getItem: () => { throw new Error('storage denied'); } }));
  assert.throws(() => saveClosureIntent({ actor, scope: 'account-closure', key, facts: { confirmation: 'CLOSE_MY_ACCOUNT' } }, { getItem: () => null, setItem: () => {} }));
});

test('BFF and Native closure operations retain exact path, confirmation and key', async t => {
  const before = globalThis.fetch; t.after(() => { globalThis.fetch = before; });
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return Response.json({ closure_request_id: actor }, { status: options.method === 'POST' ? 202 : 200 }); };
  const bff = new AccountPlatformBffClient(); bff.csrfToken = 'controlled-csrf';
  await bff.accountClosure(); await bff.createAccountClosure('CLOSE_MY_ACCOUNT', key);
  assert.equal(calls[0].url, '/api/account/v1/me/account-closure-request');
  assert.equal(calls[1].url, '/api/account/v1/me/account-closure-requests');
  assert.equal(calls[1].options.headers['Idempotency-Key'], key);
  assert.deepEqual(JSON.parse(calls[1].options.body), { confirmation: 'CLOSE_MY_ACCOUNT' });
  const nativeCalls = [];
  const native = new AccountPlatformDesktopClient({ request: async input => { nativeCalls.push(input); return { status: input.method === 'POST' ? 202 : 200, body: {} }; } });
  await native.accountClosure(); await native.createAccountClosure('CLOSE_MY_ACCOUNT', key);
  assert.equal(nativeCalls[0].resource, '/v1/me/account-closure-request');
  assert.equal(nativeCalls[1].resource, '/v1/me/account-closure-requests');
  assert.equal(nativeCalls[1].idempotencyKey, key);
});

test('admin recovery uses original target and the CSRF mutation boundary', async t => {
  const before = globalThis.fetch; t.after(() => { globalThis.fetch = before; });
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return Response.json({}); };
  const client = new AdminPlatformClient(); client.csrfToken = 'controlled-admin-csrf';
  await client.accountClosures(); await client.retryAccountClosure(other, 'Controlled original reason', key);
  assert.equal(calls[0].url, '/api/admin/account/admin/v1/account-closure-requests');
  assert.equal(calls[1].url, `/api/admin/account/admin/v1/account-closure-requests/${other}/actions/retry`);
  assert.equal(calls[1].options.headers['Idempotency-Key'], key);
  assert.equal(calls[1].options.headers['X-CSRF-Token'], 'controlled-admin-csrf');
});

test('closed-grant evidence preserves int64 version and money; batch checks every target', async () => {
  const parsed = parseClosedGrantsJSON('{"subject_lifecycle_version":9223372036854775807,"amount_microcredits":9007199254740993}');
  assert.equal(parsed.subject_lifecycle_version, '9223372036854775807');
  assert.equal(parsed.amount_microcredits, '9007199254740993');
  const projection = { organization_id: other, grant_id: grant, member_user_id: actor, package_id: actor, component_id: actor, status: 'RECLAIMED_ACCOUNT_CLOSED', membership_version: 2, allocation_version: 2, original_microcredits: '10', available_microcredits: '0', reserved_microcredits: '0', used_microcredits: '0', expired_microcredits: '0', revoked_microcredits: '10' };
  const receipt = { grant: projection, amount_microcredits: '10', subject_lifecycle_version: '9223372036854775807', replayed: true };
  const calls = [];
  let reply = receipt;
  const client = new OrganizationClient({ request: async (area, path, options) => { calls.push({ area, path, options }); return reply; } });
  await client.reclaimClosedGrant(other, grant, 'Controlled reason', key);
  assert.equal(calls[0].path, `/v1/organizations/${other}/account-closed-grants/${grant}/reclaim`);
  reply = { items: [{ grant_id: grant, status: 'SUCCEEDED', result: receipt }] };
  await client.reclaimClosedGrants(other, [grant], 'Controlled reason', key);
  assert.deepEqual(calls[1].options.expectedStatus, [207]);
  reply = { items: [{ grant_id: actor, status: 'FAILED', code: 'ACCOUNT_CLOSED_GRANT_RECLAIM_REJECTED' }] };
  await assert.rejects(client.reclaimClosedGrants(other, [grant], 'Controlled reason', key));
  reply = { items: [{ grant_id: grant, status: 'FAILED', code: 'COMMAND_RESULT_UNKNOWN' }] };
  await assert.rejects(client.reclaimClosedGrants(other, [grant], 'Controlled reason', key), 'unknown item cannot clear original batch proof');
});

test('all closure states, stages and recovery labels are explicit in every language', () => {
  for (const language of ['zh', 'en', 'ja']) {
    assert.equal(Object.keys(accountClosureCopy[language].statuses).length, 5);
    assert.equal(Object.keys(accountClosureCopy[language].stages).length, 6);
    for (const copy of [accountClosureCopy[language], accountClosedGrantCopy[language]]) {
      assert.equal(typeof copy.recoveryUnavailable, 'string'); assert.ok(copy.recoveryPending);
    }
  }
});
