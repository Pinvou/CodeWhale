import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const id = digit => `${digit.repeat(8)}-${digit.repeat(4)}-7${digit.repeat(3)}-8${digit.repeat(3)}-${digit.repeat(12)}`;

function validator(contract, schemaName) {
  const document = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${schemaName}`, $defs: contract.components.schemas }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

test('Token Account provisioning recovery endpoints and receipts are closed', async () => {
  const contract = YAML.parse(await readFile(path.join(root, 'contracts/openapi/admin.yaml'), 'utf8'));
  const matrices = new Map([
    ['/token/admin/v1/commands/by-idempotency-key', ['200', '400', '403', '404', '503']],
    ['/token/admin/v1/commands/{command_reference}/recovery-actions', ['200', '202', '400', '401', '403', '404', '409', '422', '503', '504']],
    ['/billing/internal/v1/token-account-wallet-provisioning-receipts', ['200', '201', '401', '403', '409', '422', '503']],
    ['/billing/internal/v1/token-account-wallet-provisioning-receipts/{command_reference}', ['200', '401', '403', '404', '503']],
    ['/account/internal/v1/token-account-authority/snapshot', ['200', '400', '401', '403', '503']],
  ]);
  for (const [route, statuses] of matrices) {
    const operation = contract.paths[route]?.get || contract.paths[route]?.post;
    assert.ok(operation, route);
    for (const status of statuses) assert.ok(operation.responses[status], `${route} ${status}`);
  }

  const validateReceipt = validator(contract, 'TokenAccountWalletProvisioningReceipt');
  const succeeded = {
    command_reference: id('1'), owner_type: 'PERSONAL', owner_id: id('2'), request_digest: 'a'.repeat(64),
    status: 'SUCCEEDED', wallet_id: id('3'), stable_error_code: null, attempt_count: 1,
    created_at: '2026-09-06T00:00:00Z', completed_at: '2026-09-06T00:00:01Z',
  };
  assert.equal(validateReceipt(succeeded), true, JSON.stringify(validateReceipt.errors));
  for (const malformed of [
    { ...succeeded, stable_error_code: 'FAILED' },
    { ...succeeded, completed_at: null },
    { ...succeeded, secret: 'must-not-pass' },
    { ...succeeded, status: 'PENDING', wallet_id: id('3'), completed_at: null },
    { ...succeeded, status: 'FAILED', wallet_id: null, stable_error_code: 'CREDENTIAL_FAILED' },
  ]) assert.equal(validateReceipt(malformed), false, JSON.stringify(malformed));

  const failed = { ...succeeded, status: 'FAILED', wallet_id: null, stable_error_code: 'WALLET_PROVISIONING_REJECTED' };
  assert.equal(validateReceipt(failed), true, JSON.stringify(validateReceipt.errors));
});

test('recovery action status invariants reject ambiguous or secret-bearing results', async () => {
  const contract = YAML.parse(await readFile(path.join(root, 'contracts/openapi/admin.yaml'), 'utf8'));
  const validate = validator(contract, 'TokenAccountRecoveryActionResult');
  const requeued = {
    action_id: id('4'), command_reference: id('1'), command_type: 'TOKEN_ACCOUNT_CREATE', action: 'REQUEUE',
    status: 'SUCCEEDED', recovery_state: 'PENDING', recovery_epoch: 2, stable_error_code: null,
    audit_reference: id('5'), created_at: '2026-09-06T00:00:00Z', completed_at: '2026-09-06T00:00:01Z',
  };
  assert.equal(validate(requeued), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...requeued, action: 'CANCEL', recovery_state: 'FAILED' }), true, JSON.stringify(validate.errors));
  for (const malformed of [
    { ...requeued, completed_at: null },
    { ...requeued, stable_error_code: 'FAILED' },
    { ...requeued, recovery_state: 'MANUAL_REVIEW' },
    { ...requeued, status: 'FAILED', recovery_epoch: null, recovery_state: 'MANUAL_REVIEW', stable_error_code: 'TOKEN_FAILED' },
    { ...requeued, worker_id: 'private-worker' },
  ]) assert.equal(validate(malformed), false, JSON.stringify(malformed));
});

test('Account authority snapshots expose a closed four-mode evidence contract', async () => {
  const contract = YAML.parse(await readFile(path.join(root, 'contracts/openapi/admin.yaml'), 'utf8'));
  const validateRequest = validator(contract, 'TokenOwnerAuthorityRequest');
  const validateDecision = validator(contract, 'TokenOwnerAuthorityDecision');
  const request = {
    authority_schema_version: '1', mode: 'T3_CREATOR_RECHECK', identity_issuer: null, identity_subject: null, identity_subject_sha256: null,
    actor_user_id: '018f0c7a-6b5d-7e3f-8a21-1234567890ab', owner_type: 'ORGANIZATION', owner_id: '018f0c7a-6b5d-7e3f-aa21-1234567890ad',
    required_permissions: ['platform.tokens.manage', 'platform.tokens.owner-candidates.read'], command_reference: '018f0c7a-6b5d-7e3f-9a21-1234567890ac',
    command_request_digest: 'a'.repeat(64), expected_platform_authorization_version: '13', expected_actor_version: '7', expected_owner_version: '11',
    expected_actor_identity_digest: 'b'.repeat(64), expected_owner_identity_digest: 'c'.repeat(64), authority_request_sha256: 'af5fe3148c2d352552c1b54b62f70b16182ed469bd061aee30e5b573b75454ac',
  };
  assert.equal(validateRequest(request), true, JSON.stringify(validateRequest.errors));
  for (const malformed of [
    { ...request, actor_user_id: null },
    { ...request, identity_subject: 'private-subject' },
    { ...request, required_permissions: [...request.required_permissions].reverse() },
    { ...request, expected_owner_identity_digest: null },
    { ...request, private_claim: 'must-not-pass' },
  ]) assert.equal(validateRequest(malformed), false, JSON.stringify(malformed));

  const response = {
    authority_schema_version: '1', mode: request.mode, decision: 'ALLOW', reason_code: null, actor_user_id: request.actor_user_id,
    expected_platform_authorization_version: '13', observed_platform_authorization_version: '13', expected_actor_version: '7', expected_actor_identity_digest: 'b'.repeat(64),
    observed_actor_version: '7', actor_status: 'ACTIVE', actor_identity_digest: 'b'.repeat(64), expected_owner_version: '11', expected_owner_identity_digest: 'c'.repeat(64),
    observed_owner_version: '11', owner_status: 'ACTIVE', owner_identity_digest: 'c'.repeat(64), command_reference: request.command_reference,
    command_request_digest: request.command_request_digest, authority_request_sha256: request.authority_request_sha256, observed_at: '2026-09-06T00:00:00Z',
  };
  assert.equal(validateDecision(response), true, JSON.stringify(validateDecision.errors));
  assert.equal(validateDecision({ ...response, reason_code: 'AUTHORIZATION_REVOKED' }), false);
  assert.equal(validateDecision({ ...response, decision: 'DENY', reason_code: null }), false);
  assert.equal(validateDecision({ ...response, identity_subject: 'must-not-leak' }), false);
});
