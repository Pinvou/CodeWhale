import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

const validator = (contract, schemaName) => {
  const document = JSON.parse(JSON.stringify({
    $schema: 'https://json-schema.org/draft/2020-12/schema',
    $ref: `#/$defs/${schemaName}`,
    $defs: contract.components.schemas,
  }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
};

test('Token overview and Billing price projection remain separate read contracts', async () => {
  const [contractSource, tokenService, billingService, portal, tokenMain, billingMain] = await Promise.all([
    read('contracts/openapi/admin.yaml'),
    read('internal/token/service.go'),
    read('internal/billing/service.go'),
    read('internal/portal/service.go'),
    read('cmd/token-gateway/main.go'),
    read('cmd/billing-service/main.go'),
  ]);
  const contract = YAML.parse(contractSource);
  const tokenPath = contract.paths['/token/admin/v1/operations-overview']?.get;
  const billingPath = contract.paths['/billing/admin/v1/models/{model_id}/price-projection']?.get;
  assert.ok(tokenPath && billingPath);
  for (const status of ['200', '400', '403', '503', '504']) {
    assert.ok(tokenPath.responses[status], `Token overview lacks ${status}`);
    assert.ok(billingPath.responses[status], `Billing projection lacks ${status}`);
  }
  assert.match(tokenService, /RequireAnyAnd\(\[\]string\{"platform\.models\.read"\}, \[\]string\{"platform\.tokens\.read", "platform\.usage\.read"\}\)/);
  assert.match(billingService, /Require\("platform\.pricing\.read"\).*adminModelPriceProjection/);
  assert.match(portal, /"\/admin\/v1\/operations-overview"/);
  assert.match(portal, /parts\[4\] == "price-projection"/);
  assert.match(tokenMain, /TOKEN_ADMIN_OPERATIONS_OVERVIEW_ENABLED/);
  assert.match(billingMain, /BILLING_ADMIN_MODEL_PRICE_PROJECTION_ENABLED/);
  assert.doesNotMatch(await read('internal/token/operations_overview.go'), /pricing_versions|billing_protocol_formula_bindings|http\.NewRequest/);
  assert.doesNotMatch(await read('internal/billing/model_price_projection.go'), /model_catalog_entries|routing_policies|route_provisionings|usage_requests|TOKEN_GATEWAY/);
});

test('Token overview schema closes metrics, UTC window and denominator semantics', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const schema = contract.components.schemas.TokenOperationsOverview;
  assert.equal(schema.additionalProperties, false);
  assert.deepEqual(schema.required, ['as_of', 'window', 'metrics']);
  assert.equal(schema.properties.metrics.minItems, 11);
  assert.equal(schema.properties.metrics.maxItems, 11);
  assert.equal(contract.components.schemas.TokenOperationsOverviewWindow.properties.timezone.const, 'UTC');
  const metric = contract.components.schemas.TokenOperationsOverviewMetric;
  assert.equal(metric.additionalProperties, false);
  assert.deepEqual(metric.properties.aggregation.enum, ['POINT_IN_TIME', 'WINDOW']);
  assert.deepEqual(metric.properties.denominator_key.enum, ['stable_route_count']);
  const implementation = await read('internal/token/operations_overview.go');
  assert.match(implementation, /transaction_timestamp\(\) AS as_of/);
  assert.match(implementation, /request\.started_at>=bounds\.window_from AND request\.started_at<bounds\.window_to/);
  assert.doesNotMatch(implementation, /AdminCatalogModels|AdminProvisionings|AdminUsageRequests/);
});

test('Billing projection distinguishes missing, blocked and explicit zero while retaining source version and ETag', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const schema = contract.components.schemas.ModelPriceProjection;
  assert.equal(schema.additionalProperties, false);
  assert.deepEqual(schema.properties.current.$ref, '#/components/schemas/ModelPriceProjectionPoint');
  assert.deepEqual(contract.components.schemas.ModelPriceProjectionPoint.oneOf.map(item => item.properties.coverage_status.const), ['AVAILABLE', 'MISSING', 'BLOCKED']);
  const unit = contract.components.schemas.ModelPriceProjectionUnit;
  assert.equal(unit.properties.resolved_price.minimum, 0);
  assert.ok(unit.required.includes('source_price_version_id'));
  assert.ok(contract.components.schemas.ModelPriceProjectionPrice.required.includes('space_etag'));
  assert.equal(schema.properties.source.const, 'BILLING');
  assert.match(schema.properties.snapshot_semantics.const, /NO_CROSS_DOMAIN_ATOMICITY/);

  const validate = validator(contract, 'ModelPriceProjection');
  const id = character => `${character.repeat(8)}-${character.repeat(4)}-4${character.repeat(3)}-8${character.repeat(3)}-${character.repeat(12)}`;
  const actual = {
    model_id: id('1'), protocol_code: 'CHAT', formula_version_id: id('2'), credit_unit: 'MICROCREDIT',
    as_of: '2026-09-04T08:00:00Z', source: 'BILLING',
    snapshot_semantics: 'BILLING_REPEATABLE_READ_ONLY; TOKEN_CATALOG_NOT_READ; NO_CROSS_DOMAIN_ATOMICITY',
    current: {
      effective_at: '2026-09-04T08:00:00Z', coverage_status: 'AVAILABLE',
      price: {
        price_version_id: id('3'), scope_type: 'MODEL', space_etag: 4,
        effective_from: '2026-09-01T00:00:00Z', effective_to: null,
        units: [{
          unit_code: 'INPUT_TOKEN', unit_definition_id: id('4'), unit_version: 1,
          minimum_precision: 1, price_denominator: 1000000, rounding_rule: 'CEILING', resolved_price: 0,
          configuration_type: 'INHERIT_GLOBAL', price_source_scope: 'GLOBAL', source_price_version_id: id('5'),
        }],
      },
    },
    next_change: null,
  };
  assert.equal(validate(actual), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...actual, token_catalog_version: 9 }), false, 'Billing response must stay closed and Token-free');
  assert.equal(validate({ ...actual, current: { effective_at: actual.as_of, coverage_status: 'MISSING', price: actual.current.price } }), false, 'missing must not carry a fake price');
  assert.equal(validate({ ...actual, current: { effective_at: actual.as_of, coverage_status: 'AVAILABLE' } }), false, 'available must carry resolved price evidence');
});

test('actual Token overview response validates and rejects leaked identifiers', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const validate = validator(contract, 'TokenOperationsOverview');
  const keys = contract.components.schemas.TokenOperationsOverviewMetric.properties.key.enum;
  const actual = {
    as_of: '2026-09-04T08:00:00Z',
    window: { from: '2026-09-03T08:00:00Z', to: '2026-09-04T08:00:00Z', timezone: 'UTC' },
    metrics: keys.map(key => ({
      key, value: 0, unit: 'COUNT', aggregation: key === 'usage_request_count' ? 'WINDOW' : 'POINT_IN_TIME',
      included_statuses: ['PUBLISHED'],
      ...(key === 'healthy_route_count' || key === 'route_attention_count' ? { denominator_key: 'stable_route_count' } : {}),
    })),
  };
  assert.equal(validate(actual), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...actual, token_account_id: 'secret' }), false);
});

test('authorization inventory registers the composite Token read and Billing keeps an inline exact read', async () => {
  const [inventory, billingService] = await Promise.all([
    read('deploy/authorization-entrypoints.json').then(JSON.parse),
    read('internal/billing/service.go'),
  ]);
  const token = inventory.http_policies.find(item => item.source.endsWith('/token/service.go') && item.path === '/operations-overview');
  assert.deepEqual(token.permissions, ['platform.models.read', 'platform.tokens.read', 'platform.usage.read']);
  assert.match(billingService, /admin\.GET\("\/models\/:model_id\/price-projection", s\.authorizer\.Require\("platform\.pricing\.read"\)/);
});
