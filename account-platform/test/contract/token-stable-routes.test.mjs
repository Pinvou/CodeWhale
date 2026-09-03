import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const read = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

function responseValidator(contract, schemaName) {
  const document = JSON.parse(JSON.stringify({
    $schema: 'https://json-schema.org/draft/2020-12/schema',
    $ref: `#/$defs/${schemaName}`,
    $defs: contract.components.schemas,
  }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

test('stable route write and account detail schemas validate actual JSON projections', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const id = value => `${value.repeat(8)}-${value.repeat(4)}-4${value.repeat(3)}-8${value.repeat(3)}-${value.repeat(12)}`;
  const updatedAt = '2026-09-04T12:00:00Z';
  const stableRoute = {
    route_id: id('1'), token_account_id: id('2'), model_id: id('3'), protocol_version_id: id('4'),
    protocol_code: 'CHAT', priority: 100, default_scope: 'NONE', health_status: 'UNKNOWN',
    health_diagnostic_code: 'AWAITING_INITIAL_PROBE', health_observed_at: null, health_expires_at: null,
    route_version: 1, route_generation: 1, account_version: 2, updated_at: updatedAt,
  };
  const validateWrite = responseValidator(contract, 'TokenRoute');
  assert.equal(validateWrite(stableRoute), true, JSON.stringify(validateWrite.errors));
  assert.equal(validateWrite({ ...stableRoute, model_id: null }), false, 'stable IDs must never be null');
  assert.equal(validateWrite({ ...stableRoute, model: 'legacy-text' }), false, 'stable response must not expose the legacy key');

  const stableRead = {
    route_id: id('1'), token_account_id: id('2'), model: 'stable-model', model_id: id('3'), protocol_version_id: id('4'),
    protocol_code: 'CHAT', priority: 100, default_scope: 'NONE', reference_status: 'RESOLVED', health_status: 'UNKNOWN',
    health_diagnostic_code: 'AWAITING_INITIAL_PROBE', route_version: 1, route_generation: 1, updated_at: updatedAt,
  };
  const legacyRead = {
    route_id: id('5'), token_account_id: id('2'), model: 'legacy-model', model_id: null, protocol_version_id: null,
    protocol_code: 'CHAT', priority: 50, default_scope: 'NONE', reference_status: 'LEGACY_UNRESOLVED', health_status: 'STALE',
    health_diagnostic_code: 'LEGACY_REFERENCE_UNRESOLVED', route_version: 1, route_generation: 1, updated_at: updatedAt,
  };
  const detail = {
    token_account_id: id('2'), owner_type: 'USER', owner_id: id('6'), wallet_id: id('7'), name: 'Platform account',
    status: 'ACTIVE', share_scope: 'OWNER_UNIT_ONLY', management_mode: 'PLATFORM', active_credential_count: 1,
    route_count: 1, account_version: 2, updated_at: updatedAt,
    credentials: [{ credential_id: id('8'), provider: 'PROVIDER', fingerprint: 'redacted-fingerprint', version: 1,
      status: 'ACTIVE', validation_status: 'VALID', allowed_models: ['stable-model'], validated_at: updatedAt, updated_at: updatedAt }],
    routes: [stableRead, legacyRead],
  };
  const validateDetail = responseValidator(contract, 'AdminTokenAccountDetail');
  assert.equal(validateDetail(detail), true, JSON.stringify(validateDetail.errors));
  assert.equal(validateDetail({ ...detail, credentials: [{ ...detail.credentials[0], base_url: 'https://must-not-leak.invalid' }] }), false);
});

test('stable route v2 publishes CAS, idempotency and conflict surfaces without mutating v1', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const v1 = contract.paths['/token/admin/v1/token-accounts/{token_account_id}/routes'];
  const v2 = contract.paths['/token/admin/v2/token-accounts/{token_account_id}/routes'];
  assert.equal(v1.put.requestBody.content['application/json'].schema.$ref, '#/components/schemas/LegacyTokenRouteChange');
  assert.deepEqual(Object.keys(v2.put.responses).sort(), ['200', '400', '403', '404', '409', '422', '503']);
  assert.deepEqual(Object.keys(v2.delete.responses).sort(), ['204', '400', '403', '404', '409', '422', '503']);
  for (const operation of [v2.put, v2.delete]) {
    assert.ok(operation.parameters.some(parameter => parameter.$ref === '#/components/parameters/IdempotencyKey'));
  }
  assert.equal(contract.components.schemas.TokenRouteChange.additionalProperties, false);
  assert.equal(contract.components.schemas.TokenRouteChange.properties.health_status, undefined);
  assert.equal(contract.components.schemas.StableTokenRouteRead.additionalProperties, false);
  assert.equal(contract.components.schemas.LegacyTokenRouteRead.additionalProperties, false);
});
