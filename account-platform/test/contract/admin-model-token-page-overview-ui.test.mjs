import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
import {
  modelTokenPageOverviewDrilldown,
  modelTokenPageOverviewPermissionView,
  pageOverviewInteraction,
  validateModelCatalogPageOverview,
  validateModelOperationsPageOverview,
  validateTokenResourcesPageOverview,
} from '../../../pinvou3-app/src/features/account-platform/admin-model-operations-overview.js';
import {
  createModelTokenPageOverviewRequestTracker,
  modelTokenPageOverviewCacheKey,
  planAdminModelTokenPageOverview,
} from '../../../pinvou3-app/src/features/account-platform/admin-model-token-page-overview-state.js';
import { adminModelTokenOverviewDrilldownUpdates, normalizeAdminModelTokenEntry, readAdminModelTokenOverviewResult } from '../../../pinvou3-app/src/features/account-platform/admin-navigation.js';

const metric = (key, value, aggregation, includedStatuses, denominatorKey = null) => ({ key, value, unit: 'COUNT', aggregation, included_statuses: includedStatuses, denominator_key: denominatorKey });
const model = () => ({ schema_version: '1', as_of: '2026-09-11T12:00:00Z', metrics: [
  metric('published_model_count', 2, 'POINT_IN_TIME', ['PUBLISHED']),
  metric('deprecated_model_count', 1, 'POINT_IN_TIME', ['DEPRECATED']),
  metric('model_draft_count', 3, 'POINT_IN_TIME', ['DRAFT']),
] });
const routes = (stable = 4) => ({ schema_version: '1', as_of: '2026-09-11T12:00:00Z', metrics: [
  metric('stable_route_count', stable, 'POINT_IN_TIME', ['RESOLVED']),
  metric('healthy_route_count', stable ? 3 : 0, 'POINT_IN_TIME', ['HEALTHY'], 'stable_route_count'),
  metric('route_attention_count', stable ? 1 : 0, 'POINT_IN_TIME', ['UNKNOWN', 'PROBING', 'DEGRADED', 'UNHEALTHY', 'STALE', 'EXPIRED_HEALTH'], 'stable_route_count'),
] });
const usage = () => {
  const from = Date.parse('2026-09-10T12:00:00Z');
  const points = Array.from({ length: 24 }, (_, index) => ({ from: new Date(from + index * 3600e3).toISOString(), to: new Date(from + (index + 1) * 3600e3).toISOString(), usage_request_count: index === 0 ? 5 : 0, reconciliation_attention_count: index === 1 ? 1 : 0 }));
  return { window: { from: '2026-09-10T12:00:00Z', to: '2026-09-11T12:00:00Z', timezone: 'UTC' }, metrics: [
    metric('usage_request_count', 5, 'WINDOW', ['CREATED', 'RESERVED', 'UPSTREAM_ACTIVE', 'SETTLEMENT_PENDING', 'COMMITTED', 'RELEASED', 'REVIEW']),
    metric('reconciliation_pending_count', 2, 'POINT_IN_TIME', ['SETTLEMENT_PENDING', 'REVIEW']),
    metric('reconciliation_stopped_count', 1, 'POINT_IN_TIME', ['SETTLEMENT_PENDING', 'REVIEW']),
  ], trend: { bucket_seconds: 3600, points } };
};
const provisioning = () => ({ metrics: [metric('provisioning_pending_count', 2, 'POINT_IN_TIME', ['PENDING']), metric('provisioning_attention_count', 1, 'POINT_IN_TIME', ['FAILED', 'REPAIR_REQUIRED'])] });

test('three page schemas are closed, ordered, and permission variants never leak a forbidden group', () => {
  assert.ok(validateModelCatalogPageOverview(model()));
  assert.ok(validateTokenResourcesPageOverview(routes()));
  assert.ok(validateTokenResourcesPageOverview(routes(0)));
  assert.equal(validateModelCatalogPageOverview({ ...model(), leak: true }), null);
  assert.equal(validateTokenResourcesPageOverview({ ...routes(), metrics: [...routes().metrics].reverse() }), null);
  const tokenOnly = { schema_version: '1', as_of: '2026-09-11T12:00:00Z', permission_view: 'TOKEN_ONLY', provisioning: provisioning() };
  const usageOnly = { schema_version: '1', as_of: '2026-09-11T12:00:00Z', permission_view: 'USAGE_ONLY', usage: usage() };
  const both = { ...tokenOnly, permission_view: 'TOKEN_AND_USAGE', usage: usage() };
  assert.ok(validateModelOperationsPageOverview(tokenOnly, 'TOKEN_ONLY'));
  assert.ok(validateModelOperationsPageOverview(usageOnly, 'USAGE_ONLY'));
  assert.ok(validateModelOperationsPageOverview(both, 'TOKEN_AND_USAGE'));
  assert.equal(validateModelOperationsPageOverview({ ...tokenOnly, usage: usage() }, 'TOKEN_ONLY'), null);
  assert.equal(validateModelOperationsPageOverview(both, 'USAGE_ONLY'), null);
  assert.equal(validateModelOperationsPageOverview({ ...usageOnly, usage: { ...usage(), extra: true } }, 'USAGE_ONLY'), null);
});

test('request planning is one projection per section and token-only never sends a window', async () => {
  assert.equal(modelTokenPageOverviewPermissionView(['platform.tokens.read']), 'TOKEN_ONLY');
  assert.equal(modelTokenPageOverviewPermissionView(['platform.usage.read']), 'USAGE_ONLY');
  assert.equal(modelTokenPageOverviewPermissionView(['platform.tokens.read', 'platform.usage.read']), 'TOKEN_AND_USAGE');
  assert.equal(planAdminModelTokenPageOverview('modelOperations', [], true), null);
  assert.equal(planAdminModelTokenPageOverview('models', ['platform.models.read'], false), null);
  const calls = [];
  const client = { tokenModelCatalogOverview: () => calls.push('models'), tokenResourcesPageOverview: () => calls.push('tokens'), tokenModelOperationsPageOverview: (window) => calls.push(`operations:${window}`) };
  await planAdminModelTokenPageOverview('models', ['platform.models.read'], true).load(client);
  await planAdminModelTokenPageOverview('tokenResources', ['platform.tokens.read'], true).load(client);
  await planAdminModelTokenPageOverview('modelOperations', ['platform.tokens.read'], true, '30d').load(client);
  await planAdminModelTokenPageOverview('modelOperations', ['platform.usage.read'], true, '7d').load(client);
  assert.deepEqual(calls, ['models', 'tokens', 'operations:', 'operations:7d']);
});

test('the complete 2.4.1 drilldown matrix freezes interaction, source, full updates, marker, and section', () => {
  const bounds = { from: '2026-09-10T12:00:00Z', to: '2026-09-11T12:00:00Z' };
  const bucket = { from: '2026-09-10T13:00:00Z', to: '2026-09-10T14:00:00Z' };
  const modelUpdates = lifecycle => ({ model_q: null, model_lifecycle_status: lifecycle, model_protocol_code: null, model_source_adapter: null, model_source_missing: null, model_cursor: null, model_tab: 'directory', model_overview_result: null });
  const tokenUpdates = (health, marker) => ({ token_q: null, token_owner_type: null, token_owner_id: null, token_status: null, token_health_status: health, token_credential_status: null, token_cursor: null, token_overview_result: marker });
  const provisioningUpdates = (status, marker) => ({ provisioning_q: null, provisioning_status: status, provisioning_model_id: null, provisioning_token_account_id: null, provisioning_scope_type: null, provisioning_scope_id: null, provisioning_sla_overdue: null, provisioning_cursor: null, operations_tab: 'provisionings', operations_overview_result: marker });
  const usageUpdates = (from, to, marker = null) => ({ usage_q: null, usage_execution_status: null, usage_settlement_status: null, usage_model: null, usage_token_account_id: null, usage_organization_id: null, usage_started_from: from, usage_started_to: to, usage_cursor: null, operations_tab: 'usage', operations_overview_result: marker });
  const reconciliationUpdates = marker => ({ reconciliation_q: null, reconciliation_status: null, reconciliation_severity: null, reconciliation_case_type: null, reconciliation_owner_id: null, reconciliation_sla_overdue: null, reconciliation_cursor: null, operations_tab: 'reconciliation', operations_overview_result: marker });
  const rows = [
    ['models', 'published_model_count', pageOverviewInteraction.EXACT, 'models', { lifecycle_status: 'PUBLISHED' }, modelUpdates('PUBLISHED')],
    ['models', 'deprecated_model_count', pageOverviewInteraction.EXACT, 'models', { lifecycle_status: 'DEPRECATED' }, modelUpdates('DEPRECATED')],
    ['models', 'model_draft_count', pageOverviewInteraction.NON_INTERACTIVE, undefined, undefined, null],
    ['tokenResources', 'stable_route_count', pageOverviewInteraction.NON_INTERACTIVE, undefined, undefined, null],
    ['tokenResources', 'healthy_route_count', pageOverviewInteraction.RELATED, 'tokenAccounts', { health_status: 'HEALTHY' }, tokenUpdates('HEALTHY', 'healthy_route_count')],
    ['tokenResources', 'route_attention_count', pageOverviewInteraction.RELATED, 'tokenAccounts', {}, tokenUpdates(null, 'route_attention_count')],
    ['modelOperations', 'provisioning_pending_count', pageOverviewInteraction.EXACT, 'provisionings', { status: 'PENDING' }, provisioningUpdates('PENDING', null)],
    ['modelOperations', 'provisioning_attention_count', pageOverviewInteraction.RELATED, 'provisionings', {}, provisioningUpdates(null, 'provisioning_attention_count')],
    ['modelOperations', 'usage_request_count', pageOverviewInteraction.EXACT, 'usage', { started_from: bounds.from, started_to: bounds.to }, usageUpdates(bounds.from, bounds.to)],
    ['modelOperations', 'reconciliation_pending_count', pageOverviewInteraction.RELATED, 'reconciliation', {}, reconciliationUpdates('reconciliation_pending_count')],
    ['modelOperations', 'reconciliation_stopped_count', pageOverviewInteraction.RELATED, 'reconciliation', {}, reconciliationUpdates('reconciliation_stopped_count')],
    ['modelOperations', 'usage_trend', pageOverviewInteraction.EXACT, 'usage', { started_from: bucket.from, started_to: bucket.to }, usageUpdates(bucket.from, bucket.to)],
    ['modelOperations', 'reconciliation_trend', pageOverviewInteraction.RELATED, 'reconciliation', {}, reconciliationUpdates('reconciliation_trend')],
  ];
  for (const [section, key, interaction, source, query, updates] of rows) {
    const target = modelTokenPageOverviewDrilldown(section, key, bounds, bucket);
    assert.equal(target.interaction, interaction, `${key} interaction`);
    assert.equal(target.section, section, `${key} section`);
    assert.equal(target.source, source, `${key} source`);
    assert.deepEqual(target.query, query, `${key} query`);
    assert.deepEqual(adminModelTokenOverviewDrilldownUpdates(section, target, key), updates, `${key} complete navigation update`);
    if (updates) {
      const resultKey = section === 'models' ? 'model_overview_result' : section === 'tokenResources' ? 'token_overview_result' : 'operations_overview_result';
      assert.equal(updates[resultKey], interaction === pageOverviewInteraction.RELATED ? key : null, `${key} result marker`);
    }
  }
  assert.equal(modelTokenPageOverviewDrilldown('models', 'healthy_route_count', bounds), null);
  assert.equal(adminModelTokenOverviewDrilldownUpdates('models', modelTokenPageOverviewDrilldown('tokenResources', 'healthy_route_count', bounds), 'healthy_route_count'), null);
});

test('window normalization is limited to usage-authorized operations page when new gate is on', () => {
  const location = search => ({ pathname: '/admin.html', search, hash: '' });
  const tokenOnly = normalizeAdminModelTokenEntry('modelOperations', ['modelOperations'], true, location('?section=modelOperations&model_operations_window=30d'), ['platform.tokens.read'], { modelTokenPageOverviewsEnabled: true });
  assert.equal(tokenOnly.updates.model_operations_window, null);
  const usage = normalizeAdminModelTokenEntry('modelOperations', ['modelOperations'], true, location('?section=modelOperations&model_operations_window=bad'), ['platform.usage.read'], { modelTokenPageOverviewsEnabled: true });
  assert.equal(usage.updates.model_operations_window, '24h');
  const modelPage = normalizeAdminModelTokenEntry('models', ['models'], true, location('?section=models&model_operations_window=7d'), ['platform.models.read', 'platform.usage.read'], { modelTokenPageOverviewsEnabled: true });
  assert.equal(modelPage.updates.model_operations_window, null);
  const revokedUsage = normalizeAdminModelTokenEntry('modelOperations', ['modelOperations'], true, location('?section=modelOperations&operations_overview_result=reconciliation_trend'), ['platform.tokens.read'], { modelTokenPageOverviewsEnabled: true });
  assert.equal(revokedUsage.updates.operations_overview_result, null);
  for (const [tab, marker] of [['usage', 'provisioning_attention_count'], ['provisionings', 'reconciliation_pending_count'], ['usage', 'reconciliation_stopped_count'], ['provisionings', 'reconciliation_trend']]) {
    const mismatched = normalizeAdminModelTokenEntry('modelOperations', ['modelOperations'], true, location(`?section=modelOperations&operations_tab=${tab}&operations_overview_result=${marker}`), ['platform.tokens.read', 'platform.usage.read'], { modelTokenPageOverviewsEnabled: true });
    assert.equal(mismatched.updates.operations_overview_result, null, `${marker} cannot survive ${tab}`);
  }
  const matched = normalizeAdminModelTokenEntry('modelOperations', ['modelOperations'], true, location('?section=modelOperations&operations_tab=reconciliation&operations_overview_result=reconciliation_trend'), ['platform.tokens.read', 'platform.usage.read'], { modelTokenPageOverviewsEnabled: true });
  assert.equal(Object.prototype.hasOwnProperty.call(matched.updates, 'operations_overview_result'), false);
  const gateOff = normalizeAdminModelTokenEntry('tokenResources', ['tokenResources'], true, location('?section=tokenResources&token_overview_result=healthy_route_count'), ['platform.tokens.read'], { modelTokenPageOverviewsEnabled: false });
  assert.equal(gateOff.updates.token_overview_result, null);
  assert.equal(readAdminModelTokenOverviewResult('tokenResources', location('?token_overview_result=unknown')), '');
});

test('cache identity and generations reject missing versions and late responses', () => {
  const key = modelTokenPageOverviewCacheKey({ userID: 'user', authorizationVersion: 9, section: 'modelOperations', permissionView: 'TOKEN_ONLY' });
  assert.ok(key);
  assert.equal(modelTokenPageOverviewCacheKey({ userID: 'user', authorizationVersion: 0, section: 'models', permissionView: 'MODELS' }), '');
  const tracker = createModelTokenPageOverviewRequestTracker();
  const first = tracker.begin(key); const second = tracker.begin(key);
  assert.equal(tracker.isCurrent(key, first), false);
  assert.equal(tracker.isCurrent(key, second), true);
  tracker.invalidate(key);
  assert.equal(tracker.isCurrent(key, second), false);
});

test('admin client keeps legacy endpoint while new gate-on calls use only three page endpoints', async () => {
  const client = new AdminPlatformClient();
  const requests = [];
  client.request = (_service, resource) => { requests.push(resource); return {}; };
  await client.tokenModelCatalogOverview();
  await client.tokenResourcesPageOverview();
  await client.tokenModelOperationsPageOverview('7d');
  assert.deepEqual(requests, ['/admin/v1/overviews/model-catalog', '/admin/v1/overviews/token-resources', '/admin/v1/overviews/model-operations?window=7d']);
  assert.equal(requests.some((item) => item.includes('/operations-overview')), false);
  const source = await readFile(new URL('../../../pinvou3-app/src/features/account-platform/admin-client.js', import.meta.url), 'utf8');
  assert.match(source, /tokenOperationsOverview\(window = '24h'/);
});

test('page component renders only native buttons for interactive metrics and preserves detail unload boundaries', async () => {
  const [component, workspace, styles, copy] = await Promise.all([
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/AdminModelTokenPageOverview.jsx', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/AdminModelTokenWorkspace.jsx', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/admin-platform.css', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src/shared/admin-platform-i18n.js', import.meta.url), 'utf8'),
  ]);
  assert.match(component, /target\?\.interaction === pageOverviewInteraction\.EXACT/);
  assert.match(component, /<button type="button" className="adm-model-overview-metric"/);
  assert.match(workspace, /section === 'models' && !modelID && modelTabs\.tab === 'directory'/);
  assert.match(workspace, /section === 'tokenResources' && !tokenAccountID/);
  assert.match(workspace, /section === 'modelOperations' && !operationDetailOpen/);
  assert.match(styles, /min-width:44px;min-height:44px/);
  assert.match(styles, /@media\(max-width:360px\)/);
  for (const marker of ['模型目录概览', 'Model catalog overview', 'モデルカタログ概要', '健康率不可计算', 'Health rate unavailable', '正常率を計算できません']) assert.ok(copy.includes(marker), marker);
});

test('Portal page overview gate is default-off and release ordering requires IA plus the Token projection gate', async () => {
  const [portalMain, compose, helmValues, helmSchema, helmGovernance, helmDeployment, production, planSchema] = await Promise.all([
    readFile(new URL('../../cmd/token-portal/main.go', import.meta.url), 'utf8'),
    readFile(new URL('../../deploy/compose/compose.yaml', import.meta.url), 'utf8'),
    readFile(new URL('../../deploy/helm/account-platform/values.yaml', import.meta.url), 'utf8'),
    readFile(new URL('../../deploy/helm/account-platform/values.schema.json', import.meta.url), 'utf8'),
    readFile(new URL('../../deploy/helm/account-platform/templates/_token-governance.tpl', import.meta.url), 'utf8'),
    readFile(new URL('../../deploy/helm/account-platform/templates/app-deployments.yaml', import.meta.url), 'utf8'),
    readFile(new URL('../../deploy/production/compose.business.yaml', import.meta.url), 'utf8'),
    readFile(new URL('../../contracts/deployment/model-token-release-plan.schema.json', import.meta.url), 'utf8'),
  ]);
  assert.match(portalMain, /PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED/);
  assert.match(compose, /PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED: \$\{PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED:-false\}/);
  assert.match(helmValues, /adminModelTokenPageOverviewsEnabled: false/);
  assert.match(helmSchema, /"adminModelTokenPageOverviewsEnabled":\{"type":"boolean"\}/);
  assert.match(helmGovernance, /adminModelTokenPageOverviewsEnabled[\s\S]*adminModelTokenIAEnabled/);
  assert.match(helmGovernance, /adminModelTokenPageOverviewsEnabled[\s\S]*adminPageOverviewsEnabled/);
  assert.match(helmDeployment, /PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED[\s\S]*adminModelTokenPageOverviewsEnabled/);
  assert.match(production, /TOKEN_ADMIN_PAGE_OVERVIEWS_ENABLED: "false"[\s\S]*PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED: "false"/);
  const schema = JSON.parse(planSchema);
  assert.equal(schema.properties.gates.properties.PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED.type, 'boolean');
  assert.equal(schema.properties.gates.required.includes('PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED'), true);
});
