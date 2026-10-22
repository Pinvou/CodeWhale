import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { platformRoleGrantIntent, grantablePlatformRole, requirePlatformRoles, requirePlatformRoleBinding, platformRoleFreshAuthority, platformRolePending, platformRoleFailureState } from '../src/features/account-platform/platform-role-management.js';
import { platformRoleCopy } from '../src/shared/platform-role-i18n.js';
const owner = '0199a100-0001-7000-8000-000000000101';
const user = '0199a100-0001-7000-8000-000000000202';
const role = { code: 'PLATFORM_MODEL_ADMIN', scope: 'PLATFORM', assignable: true, permissions: ['platform.models.read'] };
const now = new Date('2026-10-22T00:00:00Z');
function intent(fields = {}, selected = role) { return platformRoleGrantIntent(owner, { user_id: user, role_code: selected.code, ...fields }, selected, now); }
function binding(body, extra = {}) { return { binding_id: '0199a100-0001-7000-8000-000000000303', ...body, grant_source: 'ADMIN', created_by: owner, scope: 'PLATFORM', status: 'ACTIVE', ...extra }; }

test('actual Admin client HTTP preserves original grant command after unknown ACK and reads revoke facts', async t => {
  const requests = []; let stored; let writes = 0; let loseACK = true;
  const server = createServer(async (request, response) => {
    let text = ''; for await (const chunk of request) text += chunk;
    const body = text ? JSON.parse(text) : null;
    requests.push({ method: request.method, url: request.url, csrf: request.headers['x-csrf-token'], key: request.headers['idempotency-key'], body });
    response.setHeader('Content-Type', 'application/json');
    if (request.url === '/api/admin/account/admin/v1/roles') return response.end(JSON.stringify({ items: [role] }));
    if (request.method === 'POST') {
      if (!stored) { writes++; stored = binding(body); }
      else assert.deepEqual(body, requests.find(item => item.method === 'POST').body);
      if (loseACK) { loseACK = false; response.destroy(); return; }
      response.statusCode = 201; return response.end(JSON.stringify(stored));
    }
    if (request.method === 'DELETE') { stored = { ...stored, status: 'REVOKED', revoked_by: owner }; return response.end(JSON.stringify(stored)); }
    response.end(JSON.stringify({ items: stored ? [stored] : [] }));
  });
  server.listen(0, '127.0.0.1'); await once(server, 'listening'); t.after(() => new Promise(resolve => server.close(resolve)));
  const client = new AdminPlatformClient(`http://127.0.0.1:${server.address().port}`); client.csrfToken = 'controlled-csrf';
  assert.deepEqual(await client.platformRoles(), [role]);
  const command = intent(); platformRolePending(client, owner, command);
  await assert.rejects(client.grantPlatformRole(owner, command.body));
  assert.equal(platformRolePending(client, owner), command);
  assert.equal(platformRolePending(client, user), null);
  const result = await client.grantPlatformRole(owner, platformRolePending(client, owner).body);
  assert.equal(result.user_id, user); assert.equal(writes, 1);
  const posts = requests.filter(item => item.method === 'POST');
  assert.equal(posts.length, 2); assert.deepEqual(posts[0], posts[1]); assert.equal(posts[0].csrf, 'controlled-csrf'); assert.equal(posts[0].key, command.body.idempotency_key);
  const listed = await client.platformRoleBindings(user); assert.equal(listed[0].binding_id, result.binding_id);
  assert.ok(requests.some(item => item.url.endsWith('?user_id=' + user)));
  assert.equal((await client.revokePlatformRole(owner, result.binding_id)).status, 'REVOKED');
  assert.equal((await client.platformRoleBindings(user))[0].status, 'REVOKED');
  platformRolePending(client, owner, null); assert.equal(platformRolePending(client, owner), null);
});

test('role form and ACK contracts preserve existing scope, expiry and source restrictions', async t => {
  await t.test('protected and foreign-scope roles never become grantable', () => {
    for (const item of [{ ...role, scope: 'PERSONAL' }, { ...role, scope: 'ORGANIZATION' }, { ...role, assignable: false }, { ...role, code: 'PLATFORM_SUPER_ADMIN' }]) { assert.equal(grantablePlatformRole(item), false); assert.throws(() => intent({}, item)); }
    assert.throws(() => requirePlatformRoles({ items: [{ ...role, scope: 'ORGANIZATION' }] }));
  });
  await t.test('PII expiry reason and immutable original body', () => {
    const pii = { ...role, code: 'PLATFORM_AUDIT_PII_EXPORT' };
    assert.throws(() => intent({}, pii));
    assert.throws(() => intent({ expires_at: '2026-10-23T01:00:00Z', reason: 'controlled valid reason' }, pii));
    assert.throws(() => intent({ expires_at: '2026-10-22T01:00:00Z', reason: 'short' }, pii));
    const command = intent({ expires_at: '2026-10-22T01:00:00Z', reason: 'controlled valid reason' }, pii);
    assert.ok(Object.isFrozen(command.body)); assert.throws(() => { command.body.role_code = role.code; });
    assert.equal(intent({ expires_at: '2026-10-22T01:00:00Z', reason: '核查授权' }, pii).body.reason, '核查授权');
    assert.throws(() => intent({ expires_at: '2026-10-22T01:00:00Z', reason: '核'.repeat(170) }, pii));
  });
  await t.test('foreign target role time source and malformed ACK are rejected', () => {
    const command = intent();
    for (const extra of [{ user_id: owner }, { role_code: 'PLATFORM_IDENTITY_ADMIN' }, { grant_source: 'ACCOUNT' }, { scope: 'ORGANIZATION' }, { starts_at: '2026-10-22T00:00:01Z' }, { status: 'PENDING' }, { created_by: user }]) assert.throws(() => requirePlatformRoleBinding(binding(command.body, extra), { ...command.body, created_by: owner }));
    assert.equal(requirePlatformRoleBinding(binding(command.body, { status: 'REVOKED' }), command.body).status, 'REVOKED');
  });
  await t.test('all three UI languages have exact leaf parity without fallback', () => {
    assert.deepEqual(Object.keys(platformRoleCopy.zh), Object.keys(platformRoleCopy.en)); assert.deepEqual(Object.keys(platformRoleCopy.zh), Object.keys(platformRoleCopy.ja));
    for (const copy of Object.values(platformRoleCopy)) for (const value of Object.values(copy)) assert.ok(typeof value === 'string' && value.trim());
  });
});

test('fresh role authority rejects owner permission and live epoch changes before dispatch', async t => {
  await t.test('read-only or switched owner cannot mutate', async () => {
    for (const authorization of [{ user_id: owner, platform_permissions: ['platform.roles.read'] }, { user_id: user, platform_permissions: ['platform.roles.assign'] }]) {
      const client = { financeSessionEpoch: 0, session: async () => ({ authenticated: true }), authorization: async () => authorization };
      await assert.rejects(platformRoleFreshAuthority(client, owner, 0));
    }
  });
  await t.test('epoch change while fresh authorization is held fails before mutation', async () => {
    let release; const held = new Promise(resolve => { release = resolve; });
    const client = { financeSessionEpoch: 0, session: async () => ({ authenticated: true }), authorization: () => held };
    const result = platformRoleFreshAuthority(client, owner, 0); await Promise.resolve(); client.financeSessionEpoch++;
    release({ user_id: owner, platform_permissions: ['platform.roles.assign'] }); await assert.rejects(result);
  });
});

test('canonical pre-write reauthentication refusal is distinct from an earlier unknown write', () => {
  const refusal = { status: 401, code: 'RECENT_AUTHENTICATION_REQUIRED' };
  assert.equal(platformRoleFailureState(refusal, false, true), 'REAUTH_NOT_DISPATCHED');
  assert.equal(platformRoleFailureState(refusal, true, true), 'UNKNOWN');
  assert.equal(platformRoleFailureState({ status: 503 }, false, true), 'UNKNOWN');
  assert.equal(platformRoleFailureState(new Error('INVALID_ROLE_BINDING_ACK'), false, true), 'UNKNOWN');
  assert.equal(platformRoleFailureState({ status: 409 }, false, true), 'NOT_DISPATCHED');
  assert.equal(platformRoleFailureState({ status: 403 }, true, true), 'UNKNOWN');
});
