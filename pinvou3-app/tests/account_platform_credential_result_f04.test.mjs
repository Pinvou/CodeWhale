import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import { tokenManagementCopy } from '../src/shared/i18n.js';
import { classifyHighRiskFailure, executeHighRiskCommand, requireGovernedTokenCommandResult, validGovernedTokenCommandResult } from '../src/features/account-platform/admin-high-risk-action-state.js';

const id = '11111111-1111-4111-8111-111111111111';
const expected = { commandType: 'TOKEN_CREDENTIAL_VALIDATE', targetResourceType: 'PROVIDER_CREDENTIAL', targetResourceID: id };
const receipt = { command_reference: id, command_type: expected.commandType, policy_version: 1, actor_user_id: id, actor_permission: 'platform.tokens.credentials.validate', authorization_version: 1, idempotency_fingerprint: 'a'.repeat(64), target_resource_type: expected.targetResourceType, target_resource_id: id, status: 'FAILED', redacted_result: {}, stable_error_code: 'CREDENTIAL_PROBE_OUTCOME_UNKNOWN', approval_request_id: null, audit_request_id: id, created_at: '2026-10-08T00:00:00Z', completed_at: '2026-10-08T00:00:01Z' };

test('only the exact before-dispatch deadline error establishes no write', () => {
  const command = { idempotencyKey: 'controlled-frozen-key' };
  const actual = classifyHighRiskFailure(command, { status: 503, code: 'CREDENTIAL_REQUEST_DEADLINE_EXCEEDED' });
  assert.equal(actual.provenNoWrite, true); assert.equal(actual.unknown, false); assert.equal(actual.locked, false);
  for (const error of [{ status: 504, code: 'CREDENTIAL_REQUEST_DEADLINE_EXCEEDED' }, { status: 503, code: 'CREDENTIAL_PROBE_OUTCOME_UNKNOWN' }, { status: 503, code: 'REQUEST_TIMEOUT' }, { status: 503, code: 'CREDENTIAL_REQUEST_DEADLINE_EXCEEDED_SECRET' }, { status: 0, code: 'CREDENTIAL_REQUEST_DEADLINE_EXCEEDED' }]) {
    const unknown = classifyHighRiskFailure(command, error);
    assert.equal(unknown.provenNoWrite, false); assert.equal(unknown.unknown, true); assert.equal(unknown.locked, true); assert.equal(unknown.idempotencyKey, command.idempotencyKey);
  }
});

test('safe probe failure is terminal without asserting credential validity or retrying', async () => {
  assert.equal(validGovernedTokenCommandResult(receipt, expected), true);
  const holder = { current: { command: null, inflight: false } }; let writes = 0;
  const result = await executeHighRiskCommand(holder, { reason: 'Controlled probe verification' }, async () => { writes++; return requireGovernedTokenCommandResult(receipt, expected); });
  assert.equal(result.command.terminal, true); assert.equal(result.command.unknown, false);
  assert.equal(result.error.code, 'CREDENTIAL_PROBE_OUTCOME_UNKNOWN'); assert.equal(writes, 1);
  await executeHighRiskCommand(holder, holder.current.command.payload, async () => { writes++; });
  assert.equal(writes, 1);
});

test('exact probe exception does not accept secret-bearing, malformed or foreign receipts', () => {
  for (const stable_error_code of ['CREDENTIAL_SECRET_LEAK', 'CREDENTIAL_PROBE_OUTCOME_UNKNOWN_SECRET', 'TOKEN_ERROR', 'PROVIDER_PASSWORD_ERROR', 'credential_probe_outcome_unknown']) {
    assert.equal(validGovernedTokenCommandResult({ ...receipt, stable_error_code }, expected), false);
  }
  for (const patch of [{ completed_at: null }, { target_resource_id: '22222222-2222-4222-8222-222222222222' }, { status: 'SUCCEEDED' }]) {
    assert.equal(validGovernedTokenCommandResult({ ...receipt, ...patch }, expected), false);
  }
});

test('credential metadata, safety and actionable local diagnostics exist independently in three languages', () => {
  const codes = ['CREDENTIAL_PROBE_OUTCOME_UNKNOWN', 'CREDENTIAL_FEATURE_DISABLED', 'CREDENTIAL_PERMISSION_REVOKED', 'CREDENTIAL_LAUNCH_CANCELLED', 'CREDENTIAL_LAUNCH_EXPIRED', 'CREDENTIAL_RESULT_UNKNOWN', 'CREDENTIAL_UPDATE_FAILED', 'CREDENTIAL_VALIDATION_FAILED', 'INVALID_COMMAND_RESPONSE', 'PERMISSION_DENIED', 'COMMAND_PRECHECK_CHANGED', 'COMMAND_PAYLOAD_LOCKED'];
  const stateSource = fs.readFileSync(new URL('../src/features/account-platform/admin-high-risk-action-state.js', import.meta.url), 'utf8');
  codes.push(...[...stateSource.matchAll(/'\d{3}:([A-Z_]+)'/g)].map(match => match[1]));
  for (const language of ['zh', 'en', 'ja']) {
    const copy = tokenManagementCopy[language].modelToken;
    assert.ok(copy.credentialMetadata); assert.ok(copy.credentialSafety);
    for (const code of codes) assert.ok(copy.commandErrors[code], `${language}:${code}`);
  }
});
