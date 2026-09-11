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
  const document = JSON.parse(JSON.stringify({
    $schema: 'https://json-schema.org/draft/2020-12/schema',
    $ref: `#/$defs/${schemaName}`,
    $defs: contract.components.schemas,
  }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

const metric = (key, statuses, denominator = null, aggregation = 'POINT_IN_TIME') => ({
  key, value: 0, unit: 'COUNT', aggregation, included_statuses: statuses, denominator_key: denominator,
});

const trend = (hours = 24) => ({
  bucket_seconds: hours === 24 ? 3600 : 86400,
  points: Array.from({ length: hours === 24 ? 24 : hours / 24 }, (_, index) => ({
    from: new Date(Date.parse('2026-09-10T12:00:00Z') + index * (hours === 24 ? 3600000 : 86400000)).toISOString(),
    to: new Date(Date.parse('2026-09-10T12:00:00Z') + (index + 1) * (hours === 24 ? 3600000 : 86400000)).toISOString(),
    usage_request_count: 0,
    reconciliation_attention_count: 0,
  })),
});

const provisioning = {
  metrics: [
    metric('provisioning_pending_count', ['PENDING']),
    metric('provisioning_attention_count', ['FAILED', 'REPAIR_REQUIRED']),
  ],
};

const usage = {
  window: { from: '2026-09-10T12:00:00Z', to: '2026-09-11T12:00:00Z', timezone: 'UTC' },
  metrics: [
    metric('usage_request_count', ['CREATED', 'RESERVED', 'UPSTREAM_ACTIVE', 'SETTLEMENT_PENDING', 'COMMITTED', 'RELEASED', 'REVIEW'], null, 'WINDOW'),
    metric('reconciliation_pending_count', ['SETTLEMENT_PENDING', 'REVIEW']),
    metric('reconciliation_stopped_count', ['SETTLEMENT_PENDING', 'REVIEW']),
  ],
  trend: trend(),
};

test('both OpenAPI documents publish the three exact page projections and stable errors', async () => {
  const [admin, token] = await Promise.all([
    read('contracts/openapi/admin.yaml').then(YAML.parse),
    read('contracts/openapi/token-gateway.yaml').then(YAML.parse),
  ]);
  for (const [document, prefix] of [[admin, '/token'], [token, '']]) {
    for (const resource of ['model-catalog', 'token-resources', 'model-operations']) {
      const operation = document.paths[`${prefix}/admin/v1/overviews/${resource}`]?.get;
      assert.ok(operation, `${prefix} ${resource} projection is missing`);
      for (const status of ['200', '400', '403', '429', '503', '504']) assert.ok(operation.responses[status], `${resource} lacks ${status}`);
    }
    assert.deepEqual(document.components.schemas.ModelCatalogPageOverview.required, ['schema_version', 'as_of', 'metrics']);
    assert.deepEqual(document.components.schemas.TokenResourcesPageOverview.required, ['schema_version', 'as_of', 'metrics']);
    assert.equal(document.components.schemas.ModelTokenPageOverviewMetric.additionalProperties, false);
    assert.deepEqual(document.components.schemas.ModelTokenPageOverviewMetric.required, ['key', 'value', 'unit', 'aggregation', 'included_statuses', 'denominator_key']);
    assert.equal(document.components.schemas.ModelOperationsPageOverview.oneOf.length, 3);
    for (const code of ['TOKEN_OVERVIEW_RATE_LIMITED', 'MODEL_CATALOG_OVERVIEW_UNAVAILABLE', 'TOKEN_RESOURCES_OVERVIEW_TIMEOUT', 'MODEL_OPERATIONS_OVERVIEW_TIMEOUT']) {
      assert.ok(document.components.schemas.ModelTokenPageOverviewErrorCode.enum.includes(code));
    }
  }
});

test('point schemas freeze metric order, status sets, denominator nulls and additional properties', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const model = {
    schema_version: '1', as_of: '2026-09-11T12:00:00Z',
    metrics: [
      metric('published_model_count', ['PUBLISHED']),
      metric('deprecated_model_count', ['DEPRECATED']),
      metric('model_draft_count', ['DRAFT']),
    ],
  };
  const routes = {
    schema_version: '1', as_of: '2026-09-11T12:00:00Z',
    metrics: [
      metric('stable_route_count', ['RESOLVED']),
      metric('healthy_route_count', ['HEALTHY'], 'stable_route_count'),
      metric('route_attention_count', ['UNKNOWN', 'PROBING', 'DEGRADED', 'UNHEALTHY', 'STALE', 'EXPIRED_HEALTH'], 'stable_route_count'),
    ],
  };
  const validateModel = validator(contract, 'ModelCatalogPageOverview');
  const validateRoutes = validator(contract, 'TokenResourcesPageOverview');
  assert.equal(validateModel(model), true, JSON.stringify(validateModel.errors));
  assert.equal(validateRoutes(routes), true, JSON.stringify(validateRoutes.errors));
  assert.equal(validateModel({ ...model, usage: {} }), false);
  assert.equal(validateModel({ ...model, metrics: [...model.metrics].reverse() }), false);
  assert.equal(validateModel({ ...model, metrics: model.metrics.map(({ denominator_key, ...item }) => item) }), false);
  assert.equal(validateRoutes({ ...routes, metrics: routes.metrics.map((item, index) => index === 2 ? { ...item, included_statuses: ['DEGRADED'] } : item) }), false);
});

test('model operations schema accepts only the server-selected closed permission variants', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const validate = validator(contract, 'ModelOperationsPageOverview');
  const base = { schema_version: '1', as_of: '2026-09-11T12:00:00Z' };
  const tokenOnly = { ...base, permission_view: 'TOKEN_ONLY', provisioning };
  const usageOnly = { ...base, permission_view: 'USAGE_ONLY', usage };
  const both = { ...base, permission_view: 'TOKEN_AND_USAGE', provisioning, usage };
  for (const payload of [tokenOnly, usageOnly, both]) assert.equal(validate(payload), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...tokenOnly, usage }), false, 'token-only must not leak usage');
  assert.equal(validate({ ...usageOnly, provisioning }), false, 'usage-only must not leak Provisioning');
  assert.equal(validate({ ...both, permission_view: 'TOKEN_ONLY' }), false, 'the client cannot choose a wider shape');
  assert.equal(validate({ ...both, permission_view: 'TOKEN_AND_USAGE', extra: 1 }), false);
});

test('Token and Portal wire exact permissions, deadline propagation, rate buckets and compatibility', async () => {
  const [deadline, service, overview, portal, portalTest, tokenMain] = await Promise.all([
    read('internal/platform/requestdeadline/token_admin.go'),
    read('internal/token/service.go'),
    read('internal/token/model_token_page_overviews.go'),
    read('internal/portal/service.go'),
    read('internal/portal/token_admin_query_paths_test.go'),
    read('cmd/token-gateway/main.go'),
  ]);
  assert.match(deadline, /OverviewRead\s+Class = "overview-read"/);
  assert.match(deadline, /OverviewRead:[\s\S]*Business: 3 \* time\.Second, BFF: 5 \* time\.Second, Ingress: 7 \* time\.Second/);
  assert.match(service, /"\/overviews\/model-catalog", s\.authorizer\.RequireAny\("platform\.models\.read"\)/);
  assert.match(service, /"\/overviews\/token-resources", s\.authorizer\.RequireAny\("platform\.tokens\.read"\)/);
  assert.match(service, /"\/overviews\/model-operations", s\.authorizer\.RequireAny\("platform\.tokens\.read", "platform\.usage\.read"\)/);
  assert.match(overview, /overview:subject:[\s\S]*limit: 60/);
  assert.match(overview, /overview:source:[\s\S]*limit: 120/);
  assert.match(overview, /AdminModelOperationsOverview\(context\.Context, time\.Duration, bool, bool\)/);
  assert.match(overview, /postgresError\.Code == "57014"/);
  assert.match(portal, /"\/admin\/v1\/overviews\/model-catalog"/);
  assert.match(portal, /tokenPageOverviewAlias\(r\.Method, rawResource\)/);
  assert.match(portal, /r\.URL\.EscapedPath\(\)/);
  assert.match(portal, /requestdeadline\.ApplyToUpstream\(request, contract\)/);
  assert.match(portalTest, /overviews\/unknown/);
  assert.match(tokenMain, /TOKEN_ADMIN_PAGE_OVERVIEWS_ENABLED/);
  assert.match(service, /"\/operations-overview"/);
});

test('new gate and route index are default-off, forward-only and independently reversible', async () => {
  const [compose, values, schema, deployment, up, down, manifest] = await Promise.all([
    read('deploy/compose/compose.yaml'),
    read('deploy/helm/account-platform/values.yaml').then(YAML.parse),
    read('deploy/helm/account-platform/values.schema.json').then(JSON.parse),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
    read('migrations/token/000033_model_token_page_overview_indexes.up.sql'),
    read('migrations/token/000033_model_token_page_overview_indexes.down.sql'),
    read('migrations/manifest.json').then(JSON.parse),
  ]);
  assert.match(compose, /TOKEN_ADMIN_PAGE_OVERVIEWS_ENABLED: \$\{TOKEN_ADMIN_PAGE_OVERVIEWS_ENABLED:-false\}/);
  assert.equal(values.tokenGateway.adminPageOverviewsEnabled, false);
  assert.equal(schema.properties.tokenGateway.properties.adminPageOverviewsEnabled.type, 'boolean');
  assert.match(deployment, /TOKEN_ADMIN_PAGE_OVERVIEWS_ENABLED[\s\S]*adminPageOverviewsEnabled/);
  assert.match(up, /routing_policies_overview_health_idx[\s\S]*routing_policies\(health_status, health_expires_at, id\)[\s\S]*reference_status = 'RESOLVED'/);
  assert.match(down, /DROP INDEX IF EXISTS routing_policies_overview_health_idx/);
  const files = manifest.domains.token.map(item => item.file);
  assert.ok(files.includes('000033_model_token_page_overview_indexes.up.sql'));
  assert.ok(files.includes('000033_model_token_page_overview_indexes.down.sql'));
});

test('production performance gate freezes all page projection data and gives every SQL a cold process', async () => {
  const [performance, workflow] = await Promise.all([
    read('internal/token/operations_overview_performance_integration_test.go'),
    read('../.github/workflows/pr-check.yml'),
  ]);
  for (const marker of [
    'catalogVersions != 100', 'catalogEntries != 1000000', 'headPublished != 7000', 'headDeprecated != 2000', 'headRemoved != 1000',
    'drafts != 50000', 'draftV1 != 30000', 'draftV2 != 15000', 'draftV3 != 5000',
    'routes != 1000000', 'resolved != 800000', 'legacy != 150000', 'deleted != 50000',
    'healthyCurrent != 400000', 'healthyExpired != 80000', 'unknown != 80000', 'probing != 80000', 'degraded != 80000', 'unhealthy != 40000', 'stale != 40000',
    'routeV1 != 500000', 'routeV2 != 300000', 'routeV3 != 200000',
    'adminModelCatalogOverviewSQL', 'adminTokenResourcesOverviewSQL', 'adminModelOperationsTokenOnlySQL', 'adminModelOperationsUsageOnlySQL', 'adminModelOperationsBothSQL',
    'routing_policies_overview_health_idx', 'model_catalog_entries_status_idx', 'Execution Time',
  ]) assert.ok(performance.includes(marker), marker);
  assert.match(performance, /assertPlanRelationsAbsent\(t, plan, "usage_requests", "manual_review_cases"\)/);
  assert.match(performance, /assertPlanRelationsAbsent\(t, plan, "route_provisionings"\)/);
  assert.match(workflow, /for overview_target in token-resources model-catalog model-operations-token-only model-operations-usage-only model-operations-both legacy; do[\s\S]*docker restart[\s\S]*PINVOU_TEST_TOKEN_OVERVIEW_PERFORMANCE_TARGET="\$overview_target"/);
  assert.match(workflow, /TestModelTokenPageOverviewsPostgresIntegration/);
});
