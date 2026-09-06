import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const accountID = '0199a100-0001-7000-8000-000000000101';
const routeID = '0199a100-0001-7000-8000-000000000102';
const modelID = '0199a100-0001-7000-8000-000000000103';
const protocolID = '0199a100-0001-7000-8000-000000000104';
const hash = 'a'.repeat(64);
const instant = '2026-09-06T10:00:00Z';
const contract = YAML.parse(await readFile(path.join(root, 'contracts/openapi/admin.yaml'), 'utf8'));
function validator(name) {
  const document = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${name}`, $defs: contract.components.schemas }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}
const upsert = { token_account_id: accountID, model_id: modelID, protocol_version_id: protocolID, priority: 100, default_scope: 'NONE' };
const remove = { token_account_id: accountID, route_id: routeID };
const upsertVersions = { token_account_id: accountID, model_id: modelID, protocol_version_id: protocolID, expected_account_version: 1, expected_route_version: 0 };
const removeVersions = { token_account_id: accountID, route_id: routeID, expected_account_version: 2, expected_route_version: 1 };

test('route command prechecks and envelopes require stable identifiers and closed payloads', () => {
  const validatePrecheck = validator('TokenCommandPrecheckRequest');
  const validateEnvelope = validator('TokenCommandEnvelope');
  for (const [commandType, targetID, command, versions] of [
    ['TOKEN_ROUTE_UPSERT', accountID, upsert, upsertVersions],
    ['TOKEN_ROUTE_DELETE', routeID, remove, removeVersions],
  ]) {
    const precheck = { command_type: commandType, target_resource_type: 'TOKEN_ROUTE', target_resource_id: targetID, command };
    assert.equal(validatePrecheck(precheck), true, JSON.stringify(validatePrecheck.errors));
    const envelope = { ...precheck, reason: 'apply the reviewed stable route change', precheck_fingerprint: hash, expected_versions: { ...versions, precheck_fingerprint: hash, request_fingerprint: hash } };
    assert.equal(validateEnvelope(envelope), true, JSON.stringify(validateEnvelope.errors));
    for (const extra of ['health_status', 'upstream_model_ref', 'force', 'ignore_blocker', 'reason']) {
      assert.equal(validatePrecheck({ ...precheck, command: { ...command, [extra]: 'forbidden' } }), false, extra);
      assert.equal(validateEnvelope({ ...envelope, command: { ...command, [extra]: 'forbidden' } }), false, extra);
    }
    for (const field of Object.keys(command)) {
      const malformed = { ...command };
      delete malformed[field];
      assert.equal(validateEnvelope({ ...envelope, command: malformed }), false, `${commandType} missing ${field}`);
    }
    for (const field of Object.keys(envelope.expected_versions)) {
      const malformed = { ...envelope.expected_versions };
      delete malformed[field];
      assert.equal(validateEnvelope({ ...envelope, expected_versions: malformed }), false, `${commandType} missing ${field}`);
    }
    assert.equal(validateEnvelope({ ...envelope, target_resource_type: 'TOKEN_ACCOUNT' }), false);
    assert.equal(validateEnvelope({ ...envelope, expected_versions: { ...envelope.expected_versions, expected_account_version: 0 } }), false);
    assert.equal(validateEnvelope({ ...envelope, expected_versions: { ...envelope.expected_versions, upstream: 'forbidden' } }), false);
  }
  assert.equal(validator('TokenRouteUpsertExpectedVersions')(upsertVersions), true);
  assert.equal(validator('TokenRouteDeleteExpectedVersions')(removeVersions), true);
  assert.equal(validator('TokenRouteDeleteExpectedVersions')({ ...removeVersions, expected_route_version: 0 }), false);
});

test('route success results require a complete redacted route or immutable tombstone', () => {
  const validate = validator('TokenCommandResult');
  const base = { command_reference: routeID, command_type: 'TOKEN_ROUTE_UPSERT', policy_version: 1, actor_user_id: accountID, actor_permission: 'platform.tokens.manage', authorization_version: 1, idempotency_fingerprint: hash, target_resource_type: 'TOKEN_ROUTE', target_resource_id: accountID, status: 'SUCCEEDED', audit_request_id: 'test-route-governance', created_at: instant, completed_at: instant };
  const route = { route_id: routeID, token_account_id: accountID, model_id: modelID, protocol_version_id: protocolID, protocol_code: 'CHAT', priority: 100, default_scope: 'NONE', health_status: 'UNKNOWN', health_diagnostic_code: 'AWAITING_INITIAL_PROBE', health_observed_at: null, health_expires_at: null, route_version: 1, route_generation: 1, account_version: 2, updated_at: instant };
  const tombstone = { route_id: routeID, token_account_id: accountID, model_id: modelID, protocol_version_id: protocolID, deleted: true, deleted_route_version: 1, route_version: 2, route_generation: 1, account_version: 3, deleted_at: instant };
  for (const item of [
    { ...base, redacted_result: route },
    { ...base, command_type: 'TOKEN_ROUTE_DELETE', target_resource_id: routeID, redacted_result: tombstone },
  ]) {
    assert.equal(validate(item), true, JSON.stringify(validate.errors));
    for (const field of Object.keys(item.redacted_result)) {
      const malformed = { ...item.redacted_result };
      delete malformed[field];
      assert.equal(validate({ ...item, redacted_result: malformed }), false, `${item.command_type} missing ${field}`);
    }
    assert.equal(validate({ ...item, redacted_result: { ...item.redacted_result, secret: 'forbidden' } }), false);
    assert.equal(validate({ ...item, redacted_result: {} }), false);
    assert.equal(validate({ ...item, completed_at: null }), false);
  }
  const deletion = { ...base, command_type: 'TOKEN_ROUTE_DELETE', target_resource_id: routeID, redacted_result: tombstone };
  for (const change of [{ deleted: false }, { route_version: 1 }, { deleted_at: 'not-a-date' }, { model_id: 'display name' }]) {
    assert.equal(validate({ ...deletion, redacted_result: { ...tombstone, ...change } }), false);
  }
});

test('v2 route direct endpoints are retired sentinels, not alternate successful writers', () => {
  const resource = contract.paths['/token/admin/v2/token-accounts/{token_account_id}/routes'];
  for (const method of ['put', 'delete']) {
    assert.ok(resource[method].responses['410']);
    assert.ok(resource[method].responses['503']);
    assert.equal(Object.keys(resource[method].responses).some(status => status.startsWith('2')), false);
  }
  const lookup = contract.paths['/token/admin/v1/commands/by-idempotency-key'].get;
  assert.deepEqual(lookup.parameters.find(parameter => parameter.name === 'command_type').schema.enum, ['TOKEN_ACCOUNT_CREATE', 'TOKEN_ROUTE_UPSERT', 'TOKEN_ROUTE_DELETE']);
  assert.match(lookup.description, /another actor's result is indistinguishable/i);
});
