import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { executeHighRiskCommand, governedTokenEnvelope, prepareHighRiskCommand, requireGovernedTokenCommandResult } from '../src/features/account-platform/admin-high-risk-action-state.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { classifyTokenAccountRecoveryFailure, recoverTokenCommandByKey } from '../src/features/account-platform/admin-token-command-recovery.js';

const id = '11111111-1111-4111-8111-111111111111';
const expected = { commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: id };
const result = {
  command_reference: '22222222-2222-4222-8222-222222222222', command_type: expected.commandType,
  policy_version: 1, actor_user_id: id, actor_permission: 'platform.models.manage', authorization_version: 1,
  idempotency_fingerprint: 'a'.repeat(64), target_resource_type: expected.targetResourceType,
  target_resource_id: id, status: 'SUCCEEDED', redacted_result: {}, stable_error_code: null,
  approval_request_id: null, audit_request_id: id, created_at: '2026-10-08T00:00:00Z', completed_at: '2026-10-08T00:00:01Z',
};

test('lost transport acknowledgement remains unknown with frozen recovery key', async () => {
  const original = globalThis.fetch;
  globalThis.fetch = async () => { throw new TypeError('controlled acknowledgement loss'); };
  try {
    const client = new AdminPlatformClient();
    await assert.rejects(client.createTokenAccountRecoveryAction(id, { action: 'REQUEUE', reason: 'synthetic controlled reason', expected_recovery_epoch: 1 }, 'fixed-recovery-key'), error => {
      const failure = classifyTokenAccountRecoveryFailure(error);
      assert.equal(failure.unknown, true); assert.equal(failure.locked, true); return true;
    });
  } finally { globalThis.fetch = original; }
});

for (const error of [Object.assign(new Error('invalid result'), { code: 'INVALID_COMMAND_RESPONSE' }), { status: 503 }, { status: 504 }, { status: 409, code: 'UNRECOGNIZED_CONFLICT' }]) {
  test(`unproven recovery failure stays frozen: ${error.code || error.status}`, () => {
    const failure = classifyTokenAccountRecoveryFailure(error);
    assert.equal(failure.unknown, true); assert.equal(failure.locked, true);
  });
}
test('proven invalid request permits correcting an unsent effect', () => {
  const failure = classifyTokenAccountRecoveryFailure({ status: 400, code: 'INVALID_REQUEST' });
  assert.equal(failure.unknown, false); assert.equal(failure.provenNoWrite, true);
});
test('terminal authority does not become an unknown new operation', () => {
  const failure = classifyTokenAccountRecoveryFailure({ status: 200, code: 'COMMAND_REJECTED', finalResult: true });
  assert.equal(failure.unknown, false); assert.equal(failure.terminal, true);
});
test('no-reference recovery queries exactly the original type/key without creating', async () => {
  const calls = [];
  const client = { async governedTokenCommandByIdempotencyKey(type, key, signal) { calls.push({ type, key, signal }); return result; }, createGovernedTokenCommand() { assert.fail('recovery must not create'); } };
  const signal = new AbortController().signal;
  assert.equal(await recoverTokenCommandByKey(client, expected, 'original-fixed-key', signal), result);
  assert.deepEqual(calls, [{ type: expected.commandType, key: 'original-fixed-key', signal }]);
});
test('authoritative absence does not cause a new-key replay', async () => {
  const error = Object.assign(new Error('not found'), { status: 404, code: 'NOT_FOUND' }); let reads = 0;
  const client = { async governedTokenCommandByIdempotencyKey() { reads++; throw error; }, createGovernedTokenCommand() { assert.fail('absence is not no-write proof'); } };
  await assert.rejects(recoverTokenCommandByKey(client, expected, 'original-fixed-key'), candidate => { assert.equal(candidate.status, 202); assert.equal(candidate.code, 'COMMAND_RESULT_UNKNOWN'); assert.equal(classifyTokenAccountRecoveryFailure(candidate).unknown, true); return true; });
  assert.equal(reads, 1);
});
test('foreign-target or unfinished receipts never report success', async () => {
  for (const value of [{ ...result, target_resource_id: '33333333-3333-4333-8333-333333333333' }, { ...result, status: 'PENDING', completed_at: null }]) {
    await assert.rejects(recoverTokenCommandByKey({ governedTokenCommandByIdempotencyKey: async () => value }, expected, 'original-fixed-key'));
  }
});
test('missing invocation key performs no authority call', async () => {
  await assert.rejects(recoverTokenCommandByKey({ governedTokenCommandByIdempotencyKey() { assert.fail('missing key'); } }, expected, ''), { code: 'COMMAND_RESULT_UNKNOWN' });
});

test('actual five AuditOnly JSX recovery callbacks pass their original key and validate their target', async () => {
  const sources = ['AdminModelTokenWorkspace.jsx', 'AdminModelDetailWorkspace.jsx', 'AdminTokenResourcesWorkspace.jsx', 'AdminTokenAccountDetailWorkspace.jsx'];
  const current = { id, model_id: id, credential_id: id };
  const action = { item: current }, model = current, credential = current, accountID = id;
  const seen = [];
  for (const source of sources) {
    const text = fs.readFileSync(new URL(`../src/features/account-platform/${source}`, import.meta.url), 'utf8');
    const pattern = /recoverByIdempotencyKey=\{(async \(key, signal\) => requireGovernedTokenCommandResult\(await client\.governedTokenCommandByIdempotencyKey\('([^']+)', key, signal\), (\{[^\n]*?\})\))\}/g;
    for (const match of text.matchAll(pattern)) {
      if (!['TOKEN_MODEL_PUBLISH', 'TOKEN_MODEL_CATALOG_SYNC', 'TOKEN_ACCOUNT_STATUS_CHANGE', 'TOKEN_CREDENTIAL_VALIDATE'].includes(match[2])) continue;
      const evaluate = new Function('client', 'requireGovernedTokenCommandResult', 'action', 'model', 'credential', 'accountID', `return (${match[1]});`);
      const calls = [];
      const client = { async governedTokenCommandByIdempotencyKey(type, key, signal) {
        calls.push({ type, key, signal });
        return { ...result, command_type: type, target_resource_type: type === 'TOKEN_MODEL_CATALOG_SYNC' ? 'MODEL_CATALOG' : type === 'TOKEN_ACCOUNT_STATUS_CHANGE' ? 'TOKEN_ACCOUNT' : type === 'TOKEN_CREDENTIAL_VALIDATE' ? 'PROVIDER_CREDENTIAL' : 'MODEL', target_resource_id: type === 'TOKEN_MODEL_CATALOG_SYNC' ? 'CURRENT' : id };
      } };
      const callback = evaluate(client, requireGovernedTokenCommandResult, action, model, credential, accountID);
      const signal = new AbortController().signal;
      const receipt = await callback('original-jsx-key', signal);
      assert.equal(receipt.status, 'SUCCEEDED');
      assert.deepEqual(calls, [{ type: match[2], key: 'original-jsx-key', signal }]);
      seen.push(`${source}:${match[2]}`);
    }
  }
  assert.deepEqual(seen, ['AdminModelTokenWorkspace.jsx:TOKEN_MODEL_PUBLISH', 'AdminModelTokenWorkspace.jsx:TOKEN_MODEL_CATALOG_SYNC', 'AdminModelDetailWorkspace.jsx:TOKEN_MODEL_PUBLISH', 'AdminTokenResourcesWorkspace.jsx:TOKEN_ACCOUNT_STATUS_CHANGE', 'AdminTokenAccountDetailWorkspace.jsx:TOKEN_CREDENTIAL_VALIDATE']);
});

for (const error of [{ status: 404, code: 'NOT_FOUND' }, { status: 403, code: 'PERMISSION_DENIED' }, { status: 400, code: 'INVALID_REQUEST' }]) {
  test(`Recovery lookup cannot prove original no-write: ${error.status}`, () => {
    const failure = classifyTokenAccountRecoveryFailure(error, true);
    assert.equal(failure.unknown, true); assert.equal(failure.locked, true);
  });
}

function actualOwnerCreateExecute(client, pendingInput) {
  const text = fs.readFileSync(new URL('../src/features/account-platform/AdminTokenResourcesWorkspace.jsx', import.meta.url), 'utf8');
  const start = text.indexOf('execute={async (payload, key, signal) => {', text.indexOf('function OwnerDialog('));
  const end = text.indexOf('\n\t}} recover=', start);
  assert.ok(start >= 0 && end > start, 'current OwnerDialog callback required');
  const callback = text.slice(start + 'execute={'.length, end + '\n\t}'.length);
  return new Function('client', 'pendingInput', 'governedTokenEnvelope', 'requireGovernedTokenCommandResult', 'recoverTokenCommandByKey', `return (${callback});`)(client, pendingInput, governedTokenEnvelope, requireGovernedTokenCommandResult, recoverTokenCommandByKey);
}

for (const postFailure of [new TypeError('controlled acknowledgement loss'), Object.assign(new Error('unavailable acknowledgement'), { status: 503, code: 'DEPENDENCY_UNAVAILABLE' })]) {
  test(`actual OwnerDialog initial fallback404 stays UNKNOWN: ${postFailure.code || postFailure.name}`, async () => {
    const input = { owner_type: 'USER', owner_id: id, name: 'controlled personal token', priority: 100 };
    const receipt = { ...result, command_type: 'TOKEN_ACCOUNT_CREATE', target_resource_type: 'TOKEN_ACCOUNT', actor_permission: 'platform.tokens.manage' };
    const payload = { reason: 'controlled immutable owner creation', precheck: { action: 'TOKEN_ACCOUNT_CREATE', precheck_fingerprint: 'b'.repeat(64), expected_versions: {} } };
    const calls = []; let absent = true;
    const client = {
      async createGovernedTokenCommand(envelope, key) { calls.push({ kind: 'POST', envelope, key }); throw postFailure; },
      async governedTokenCommandByIdempotencyKey(type, key) { calls.push({ kind: 'GET', type, key }); if (absent) throw Object.assign(new Error('unconfirmed absence'), { status: 404, code: 'NOT_FOUND', requestID: 'controlled-lookup-request' }); return receipt; },
    };
    const holder = { current: null };
    const execute = actualOwnerCreateExecute(client, input);
    const original = await executeHighRiskCommand(holder, payload, (frozen, key) => execute(frozen, key), () => 'fixed-owner-operation');
    assert.equal(original.ok, false); assert.equal(original.command.unknown, true); assert.equal(original.command.locked, true); assert.equal(original.command.provenNoWrite, false);
    assert.equal(original.error.requestID, 'controlled-lookup-request');
    const frozenKey = original.command.idempotencyKey;
    assert.deepEqual(calls.map(call => call.kind), ['POST', 'GET']); assert.ok(calls.every(call => call.key === frozenKey));
    const changed = prepareHighRiskCommand(original.command, { ...payload, reason: 'changed owner creation reason' }, () => 'forbidden-new-key');
    assert.equal(changed.locked, true); assert.equal(changed.idempotencyKey, frozenKey);
    const ordinary = await executeHighRiskCommand(holder, payload, (frozen, key) => execute(frozen, key), () => 'forbidden-new-key');
    assert.equal(ordinary.recoveryRequired, true); assert.equal(calls.length, 2);
    absent = false;
    const recovered = await executeHighRiskCommand(holder, payload, (_, key) => recoverTokenCommandByKey(client, { commandType: 'TOKEN_ACCOUNT_CREATE', targetResourceType: 'TOKEN_ACCOUNT', targetResourceID: id }, key), () => 'forbidden-new-key', { recovery: true });
    assert.equal(recovered.ok, true); assert.equal(recovered.value, receipt); assert.equal(recovered.command.idempotencyKey, frozenKey);
    assert.deepEqual(calls.map(call => call.kind), ['POST', 'GET', 'GET']); assert.ok(calls.every(call => call.key === frozenKey));
  });
}

test('actual OwnerDialog initial fallback succeeds only with terminal matching receipt', async () => {
  const receipt = { ...result, command_type: 'TOKEN_ACCOUNT_CREATE', target_resource_type: 'TOKEN_ACCOUNT', actor_permission: 'platform.tokens.manage' };
  const calls = [];
  const client = { async createGovernedTokenCommand(_, key) { calls.push(['POST', key]); throw new TypeError('controlled lost acknowledgement'); }, async governedTokenCommandByIdempotencyKey(type, key) { calls.push([type, key]); return receipt; } };
  const execute = actualOwnerCreateExecute(client, { owner_type: 'USER', owner_id: id, name: 'controlled personal token', priority: 100 });
  assert.equal(await execute({ reason: 'controlled full owner creation', precheck: { precheck_fingerprint: 'b'.repeat(64), expected_versions: {} } }, 'original-owner-key'), receipt);
  assert.deepEqual(calls, [['POST', 'original-owner-key'], ['TOKEN_ACCOUNT_CREATE', 'original-owner-key']]);
});

test('actual OwnerDialog retains explicit initial no-write failure without lookup', async () => {
  const failure = Object.assign(new Error('create is inactive'), { status: 503, code: 'TOKEN_ACCOUNT_CREATE_NOT_ACTIVE' });
  const execute = actualOwnerCreateExecute({ async createGovernedTokenCommand() { throw failure; }, async governedTokenCommandByIdempotencyKey() { assert.fail('explicit no-write must not query'); } }, { owner_type: 'USER', owner_id: id });
  await assert.rejects(execute({ reason: 'controlled full owner creation', precheck: { precheck_fingerprint: 'b'.repeat(64), expected_versions: {} } }, 'original-owner-key'), error => error === failure);
});
