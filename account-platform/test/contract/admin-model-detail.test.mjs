import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

function validator(contract, schemaName) {
  const document = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${schemaName}`, $defs: contract.components.schemas }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

const id = digit => `${digit.repeat(8)}-${digit.repeat(4)}-4${digit.repeat(3)}-8${digit.repeat(3)}-${digit.repeat(12)}`;

test('model detail is a closed Token-owned safe projection', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const operation = contract.paths['/token/admin/v1/models/{model_id}']?.get;
  assert.ok(operation);
  for (const status of ['200', '400', '403', '404', '503']) assert.ok(operation.responses[status]);
  const validate = validator(contract, 'TokenAdminModelDetail');
  const actual = {
    as_of: '2026-09-04T12:00:00Z',
    model: {
      model_id: id('1'), catalog_version: 7, display_name: 'Flash', description: '', aliases: [], lifecycle_status: 'PUBLISHED', protocol_code: 'CHAT', protocol_version: 2,
      protocol_definition_id: id('2'), model_capability_version_id: id('3'), capability_version: 4, context_length: 128000, maximum_output_tokens: 8192,
      supported_modalities: ['TEXT'], tokenizer_version: 'v2', source_available: true, source_missing_confirmations: 0, force_cancel_inflight: false,
    },
    protocol: { protocol_definition_id: id('2'), protocol_code: 'CHAT', version: 2, status: 'PUBLISHED', schema_field_count: 3, etag: 'a'.repeat(64), created_at: '2026-09-01T00:00:00Z', published_at: '2026-09-02T00:00:00Z' },
    formula: { formula_version_id: id('4'), protocol_definition_id: id('2'), protocol_code: 'CHAT', version: 5, status: 'PUBLISHED', required_unit_versions: { INPUT_TOKEN: id('5') }, evaluator_version: 'SAFE_V1', etag: 'b'.repeat(64), created_at: '2026-09-02T00:00:00Z', published_at: '2026-09-03T00:00:00Z' },
  };
  assert.equal(validate(actual), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...actual, provider_id: 'private-provider' }), false);
  assert.equal(validate({ ...actual, protocol: { ...actual.protocol, request_schema: { secret: true } } }), false);
  assert.equal(validate({ ...actual, formula: { ...actual.formula, expression: { INPUT_TOKEN: 'secret' } } }), false);
  assert.equal(validate({ ...actual, formula: null }), true, JSON.stringify(validate.errors));
});

test('model version history is a closed immutable paginated projection', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const operation = contract.paths['/token/admin/v1/models/{model_id}/versions']?.get;
  assert.ok(operation);
  for (const status of ['200', '400', '403', '410', '422', '429', '503', '504']) assert.ok(operation.responses[status]);
  assert.equal(operation.responses['429'].$ref, '#/components/responses/TokenAdminQueryLimited');
  const limited = contract.components.responses.TokenAdminQueryLimited.content['application/json'].schema;
  assert.equal(limited.additionalProperties, false);
  assert.deepEqual(limited.required, ['code', 'message', 'request_id']);
  assert.deepEqual([...limited.properties.code.allOf[1].enum].sort(), ['PAGINATION_SNAPSHOT_LIMITED', 'RATE_LIMITED']);
  const validateLimited = new Ajv2020({ allErrors: true, strict: false }).compile({
    ...limited,
    properties: { ...limited.properties, code: { type: 'string', enum: limited.properties.code.allOf[1].enum } },
  });
  for (const code of ['RATE_LIMITED', 'PAGINATION_SNAPSHOT_LIMITED']) assert.equal(validateLimited({ code, message: 'limited', request_id: 'request' }), true, JSON.stringify(validateLimited.errors));
  assert.equal(validateLimited({ code: 'UNKNOWN', message: 'limited', request_id: 'request' }), false);
  assert.equal(validateLimited({ code: 'RATE_LIMITED', message: 'limited', request_id: 'request', secret: true }), false);
  const validate = validator(contract, 'TokenAdminModelVersion');
  const version = {
    catalog_version: 6, display_name: 'Flash v2', lifecycle_status: 'PUBLISHED', model_capability_version_id: id('3'), capability_version: 2,
    protocol_definition_id: id('2'), protocol_code: 'CHAT', protocol_version: 2, schema_field_count: 4, context_length: 64000,
    maximum_output_tokens: 4096, supported_modalities: ['TEXT'], tokenizer_version: 'v1', source_available: true, published_at: '2026-08-01T00:00:00Z',
  };
  assert.equal(validate(version), true, JSON.stringify(validate.errors));
  for (const privateFact of [
    { provider_id: 'private' }, { adapter_version: 'private' }, { upstream_model_ref: 'private' },
    { request_schema: { parameters: ['secret'] } }, { expression: { secret: true } }, { description: 'free text' },
  ]) assert.equal(validate({ ...version, ...privateFact }), false, JSON.stringify(privateFact));
});

test('detail UI consumes Token, Billing, Provisioning and audit as independent read sources', async () => {
  const [ui, client, navigation, portal] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/AdminModelDetailWorkspace.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../pinvou3-app/src/features/account-platform/admin-navigation.js'),
    read('internal/portal/service.go'),
  ]);
  for (const tab of ['overview', 'capabilities', 'operations', 'pricing', 'lifecycle', 'audit']) assert.match(ui, new RegExp(`'${tab}'`));
  assert.match(client, /catalogModel\(modelID, signal\)/);
  assert.match(client, /modelVersions\(modelID, filters = \{\}, signal\)/);
  assert.match(client, /modelPriceProjection\(modelID, protocolCode, formulaVersionID/);
  assert.match(ui, /client\.provisionings\(\{ \.\.\.filters, model_id: modelID \}/);
  assert.match(ui, /resource_type: 'MODEL', resource_id: modelID/);
  assert.match(ui, /client\.modelVersions\(modelID, filters, signal\)/);
  assert.match(ui, /Object\.keys\(copy\.dependencyLabels\)/);
  assert.match(ui, /impact\.price_coverage/);
  assert.match(navigation, /admin_model_details/);
  assert.match(portal, /admin_model_details/);
  assert.doesNotMatch(ui, /provider_id|upstream_model|expression/);
  assert.doesNotMatch(ui, /transitionModel|saveModelDraft|publishModelDraft/);
});
