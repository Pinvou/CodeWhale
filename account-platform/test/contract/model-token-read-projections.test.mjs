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
  assert.deepEqual(tokenPath.parameters.map(({ name, required, schema }) => ({ name, required, schema })), [
    { name: 'window', required: true, schema: { type: 'string', enum: ['24h', '7d', '30d'] } },
  ]);
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

test('Token overview schema closes metrics, UTC window, trends and denominator semantics', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const schema = contract.components.schemas.TokenOperationsOverview;
  assert.equal(schema.additionalProperties, false);
  assert.deepEqual(schema.required, ['as_of', 'window', 'metrics', 'trend']);
  assert.equal(schema.properties.metrics.minItems, 11);
  assert.equal(schema.properties.metrics.maxItems, 11);
  assert.equal(contract.components.schemas.TokenOperationsOverviewWindow.properties.timezone.const, 'UTC');
  const metric = contract.components.schemas.TokenOperationsOverviewMetric;
  assert.equal(metric.additionalProperties, false);
  assert.deepEqual(metric.properties.aggregation.enum, ['POINT_IN_TIME', 'WINDOW']);
  assert.deepEqual(metric.properties.denominator_key.enum, ['stable_route_count']);
  const trend = contract.components.schemas.TokenOperationsOverviewTrend;
  assert.equal(trend.additionalProperties, false);
  assert.deepEqual(trend.properties.bucket_seconds.enum, [3600, 86400]);
  assert.equal(trend.properties.points.maxItems, 30);
  const implementation = await read('internal/token/operations_overview.go');
  assert.match(implementation, /transaction_timestamp\(\) AS as_of/);
  assert.match(implementation, /request\.started_at>=bucket_config\.window_from AND request\.started_at<bucket_config\.window_to/);
  assert.match(implementation, /review\.domain='TOKEN' AND review\.case_type='USAGE_RECONCILIATION_REVIEW' AND review\.resource_type='USAGE_REQUEST'/);
  assert.match(implementation, /EXISTS\(SELECT 1 FROM usage_requests request WHERE request\.id=review\.resource_id\)/);
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
    trend: {
      bucket_seconds: 3600,
      points: Array.from({ length: 24 }, (_, index) => ({
        from: new Date(Date.parse('2026-09-03T08:00:00Z') + index * 3600000).toISOString(),
        to: new Date(Date.parse('2026-09-03T08:00:00Z') + (index + 1) * 3600000).toISOString(),
        usage_request_count: 0,
        reconciliation_attention_count: 0,
      })),
    },
  };
  assert.equal(validate(actual), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...actual, token_account_id: 'secret' }), false);
  assert.equal(validate({ ...actual, trend: { ...actual.trend, worker_id: 'secret' } }), false);
});

test('catalog difference is a closed redacted top-N projection', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const validate = validator(contract, 'CatalogDifference');
  const actual = {
    baseline_catalog_version: 9,
    target_catalog_version: 8,
    target_observed_version: null,
    added_count: 1,
    modified_count: 1,
    removed_count: 1,
    total_count: 3,
    truncated: false,
    items: [
      { change_type: 'ADDED', model_id: null, display_name: 'New model' },
      { change_type: 'MODIFIED', model_id: '00000000-0000-7000-8000-000000000001', display_name: 'Changed model' },
      { change_type: 'REMOVED', model_id: '00000000-0000-7000-8000-000000000002', display_name: 'Removed model' },
    ],
  };
  assert.equal(validate(actual), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...actual, target_catalog_version: null, target_observed_version: 'a'.repeat(64), added_count: 1, modified_count: 0, removed_count: 0, total_count: 1, items: [{ ...actual.items[0], model_id: null }] }), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...actual, upstream_model_ref: 'provider-secret-name' }), false);
  assert.equal(validate({ ...actual, items: [{ ...actual.items[0], provider_id: 'private-provider' }] }), false);
  assert.equal(validate({ ...actual, target_catalog_version: null }), false, 'exactly one target discriminator is required');
  assert.equal(validate({ ...actual, target_observed_version: 'not-a-fingerprint' }), false, 'provider observations are opaque fingerprints');
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

test('operations overview rollout gate is default-off in Portal and deployment manifests', async () => {
  const [portalMain, compose, values, deployment] = await Promise.all([
    read('cmd/token-portal/main.go'),
    read('deploy/compose/compose.yaml'),
    read('deploy/helm/account-platform/values.yaml'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
  ]);
  assert.match(portalMain, /adminModelOperationsOverviewEnabled := false[\s\S]*PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED[\s\S]*strconv\.ParseBool/);
  assert.match(compose, /PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED:-false/);
  assert.equal(YAML.parse(values).portal.adminModelOperationsOverviewEnabled, false);
  assert.match(deployment, /PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED[\s\S]*adminModelOperationsOverviewEnabled/);
});

test('operations overview performance gate freezes indexes, cold restart and no-SKIP execution', async () => {
  const [up, down, workflow, performance] = await Promise.all([
    read('migrations/token/000028_operations_overview_indexes.up.sql'),
    read('migrations/token/000028_operations_overview_indexes.down.sql'),
    read('../.github/workflows/pr-check.yml'),
    read('internal/token/operations_overview_performance_integration_test.go'),
  ]);
  assert.match(up, /usage_requests_operations_overview_started_idx[\s\S]*usage_requests\(started_at, id\)/);
  assert.match(up, /manual_review_cases_token_usage_overview_created_idx[\s\S]*domain\s*=\s*'TOKEN'[\s\S]*case_type\s*=\s*'USAGE_RECONCILIATION_REVIEW'[\s\S]*resource_type\s*=\s*'USAGE_REQUEST'/);
  assert.match(down, /DROP INDEX IF EXISTS usage_requests_operations_overview_started_idx/);
  assert.match(workflow, /PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_PHASE=seed[\s\S]*docker restart[\s\S]*PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_PHASE=measure/);
  assert.match(workflow, /! grep -F -- '--- SKIP: TestTokenOperationsOverviewPerformancePostgresIntegration'/);
  assert.match(performance, /EXPLAIN \(ANALYZE, BUFFERS, FORMAT JSON\)/);
  assert.match(performance, /executionMS > 3000/);
  assert.doesNotMatch(performance, /DROP CONSTRAINT manual_review_cases_domain_check/);
  assert.match(performance, /if phase == "measure"[\s\S]*measureTokenOverviewPlans\(t, ctx, connection\)[\s\S]*return/);
  assert.match(performance, /assertTokenOverviewPerformanceSeed\(t, ctx, connection\)[\s\S]*VACUUM \(ANALYZE\)/);
  assert.match(performance, /usageWindow != 700000[\s\S]*usageHistorical != 300000[\s\S]*committed != 850000[\s\S]*pending != 100000[\s\S]*review != 50000/);
  assert.match(performance, /relevantReview != 60000[\s\S]*decoyReview != 40000[\s\S]*reviewWindow != 42000/);
  assert.match(performance, /otherDomain != 0[\s\S]*otherCase != 20000[\s\S]*otherResource != 20000[\s\S]*overlappingDecoy != 0/);
  assert.match(performance, /bufferFields == 0 \|\| bufferEvidence <= 0/);
});

test('catalog synchronization binds the governed command to one checked observation and transaction', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const validateEnvelope = validator(contract, 'TokenCommandEnvelope');
  const validateResult = validator(contract, 'TokenCommandResult');
  const envelope = {
    command_type: 'TOKEN_MODEL_CATALOG_SYNC', reason: 'synchronize the reviewed provider catalog',
    target_resource_type: 'MODEL_CATALOG', target_resource_id: 'CURRENT', command: {},
    precheck_fingerprint: '1'.repeat(64),
    expected_versions: { expected_catalog_version: 7, adapter_observed_version: '2'.repeat(64), catalog_diff_fingerprint: '3'.repeat(64), request_fingerprint: '4'.repeat(64) },
  };
  assert.equal(validateEnvelope(envelope), true, JSON.stringify(validateEnvelope.errors));
  assert.equal(validateEnvelope({ ...envelope, command: { rediscover: true } }), false);
  assert.equal(validateEnvelope({ ...envelope, expected_versions: { ...envelope.expected_versions, extra: 1 } }), false);
  const result = {
    command_reference: '0199a136-0001-7000-8000-000000000001', command_type: 'TOKEN_MODEL_CATALOG_SYNC', policy_version: 1,
    actor_user_id: '0199a136-0001-7000-8000-000000000002', actor_permission: 'platform.models.sync', authorization_version: 7,
    idempotency_fingerprint: '5'.repeat(64), target_resource_type: 'MODEL_CATALOG', target_resource_id: 'CURRENT', status: 'SUCCEEDED',
    redacted_result: { catalog_version: 8, changed: true }, audit_request_id: 'catalog-sync-test', created_at: '2026-09-05T00:00:00Z', completed_at: '2026-09-05T00:00:01Z',
  };
  assert.equal(validateResult(result), true, JSON.stringify(validateResult.errors));
  assert.equal(validateResult({ ...result, redacted_result: { ...result.redacted_result, provider_response: 'private' } }), false);
  const [governance, catalog, legacy, integration] = await Promise.all([
    read('internal/token/catalog_sync_governance.go'), read('internal/token/catalog.go'), read('internal/token/catalog_http.go'),
    read('internal/token/catalog_sync_observation_integration_test.go'),
  ]);
  assert.match(governance, /lockCatalogPublication[\s\S]*catalogPreviewFromObservation[\s\S]*tokenCommandExpectedVersionsMatch[\s\S]*recheckTokenRouteActivation[\s\S]*syncCatalogTx[\s\S]*completeRuntimeTokenCommandTx/);
  assert.match(catalog, /syncCatalogTx[\s\S]*exact normalized provider observation/);
  assert.doesNotMatch(legacy, /catalogSync\.Sync\(r\.Context\(\)\)/);
  assert.match(integration, /SAFE_ROLLBACK[\s\S]*TOKEN_COMMAND_GOVERNANCE_INACTIVE[\s\S]*assertCatalogSyncFactCounts/);
  assert.match(integration, /Epoch-only drift[\s\S]*TOKEN_COMMAND_GOVERNANCE_INACTIVE[\s\S]*assertCatalogSyncFactCounts/);
  assert.equal(contract.paths['/token/admin/v1/models:sync'], undefined);
});
