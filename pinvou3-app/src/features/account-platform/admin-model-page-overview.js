const modelTokenPageOverviewSections = Object.freeze({ models: ['published_model_count', 'deprecated_model_count', 'model_draft_count'], tokenResources: ['stable_route_count', 'healthy_route_count', 'route_attention_count'], modelOperationsToken: ['provisioning_pending_count', 'provisioning_attention_count'], modelOperationsUsage: ['usage_request_count', 'reconciliation_pending_count', 'reconciliation_stopped_count'] });
const pageOverviewMetricContracts = Object.freeze({
  published_model_count: ['POINT_IN_TIME', ['PUBLISHED'], null],
  deprecated_model_count: ['POINT_IN_TIME', ['DEPRECATED'], null],
  model_draft_count: ['POINT_IN_TIME', ['DRAFT'], null],
  stable_route_count: ['POINT_IN_TIME', ['RESOLVED'], null],
  healthy_route_count: ['POINT_IN_TIME', ['HEALTHY'], 'stable_route_count'],
  route_attention_count: ['POINT_IN_TIME', ['UNKNOWN', 'PROBING', 'DEGRADED', 'UNHEALTHY', 'STALE', 'EXPIRED_HEALTH'], 'stable_route_count'],
  provisioning_pending_count: ['POINT_IN_TIME', ['PENDING'], null],
  provisioning_attention_count: ['POINT_IN_TIME', ['FAILED', 'REPAIR_REQUIRED'], null],
  usage_request_count: ['WINDOW', ['CREATED', 'RESERVED', 'UPSTREAM_ACTIVE', 'SETTLEMENT_PENDING', 'COMMITTED', 'RELEASED', 'REVIEW'], null],
  reconciliation_pending_count: ['POINT_IN_TIME', ['SETTLEMENT_PENDING', 'REVIEW'], null],
  reconciliation_stopped_count: ['POINT_IN_TIME', ['SETTLEMENT_PENDING', 'REVIEW'], null],
});

function exactKeys(value, keys) {
  return value && typeof value === 'object' && !Array.isArray(value)
    && Object.keys(value).sort().join(',') === [...keys].sort().join(',');
}

function validTimestamp(value) {
  return typeof value === 'string' && !Number.isNaN(Date.parse(value));
}

function validatePageOverviewMetrics(metrics, expectedKeys) {
  if (!Array.isArray(metrics) || metrics.length !== expectedKeys.length) return null;
  const result = new Map();
  for (let index = 0; index < expectedKeys.length; index += 1) {
    const metric = metrics[index];
    const expectedKey = expectedKeys[index];
    const contract = pageOverviewMetricContracts[expectedKey];
    if (!exactKeys(metric, ['key', 'value', 'unit', 'aggregation', 'included_statuses', 'denominator_key'])
      || metric.key !== expectedKey || !Number.isSafeInteger(metric.value) || metric.value < 0
      || metric.unit !== 'COUNT' || metric.aggregation !== contract[0]
      || JSON.stringify(metric.included_statuses) !== JSON.stringify(contract[1])
      || metric.denominator_key !== contract[2]) return null;
    result.set(metric.key, metric);
  }
  return result;
}

export function validateModelCatalogPageOverview(value) {
  if (!exactKeys(value, ['schema_version', 'as_of', 'metrics']) || value.schema_version !== '1' || !validTimestamp(value.as_of)) return null;
  const metrics = validatePageOverviewMetrics(value.metrics, modelTokenPageOverviewSections.models);
  return metrics ? { ...value, metrics } : null;
}

export function validateTokenResourcesPageOverview(value) {
  if (!exactKeys(value, ['schema_version', 'as_of', 'metrics']) || value.schema_version !== '1' || !validTimestamp(value.as_of)) return null;
  const metrics = validatePageOverviewMetrics(value.metrics, modelTokenPageOverviewSections.tokenResources);
  if (!metrics || metrics.get('healthy_route_count').value + metrics.get('route_attention_count').value !== metrics.get('stable_route_count').value) return null;
  return { ...value, metrics };
}

function validateUsageWindow(value, asOf) {
  if (!exactKeys(value, ['from', 'to', 'timezone']) || value.timezone !== 'UTC'
    || !validTimestamp(value.from) || !validTimestamp(value.to)
    || Date.parse(value.from) >= Date.parse(value.to) || Date.parse(value.to) !== Date.parse(asOf)) return null;
  const duration = Date.parse(value.to) - Date.parse(value.from);
  if (duration !== 24 * 3600e3) return null;
  return value;
}

function validateUsageTrend(value, window, usageTotal) {
  if (!exactKeys(value, ['bucket_seconds', 'points']) || value.bucket_seconds !== 3600 || !Array.isArray(value.points)) return null;
  const duration = Date.parse(window.to) - Date.parse(window.from);
  const expectedPoints = duration / (value.bucket_seconds * 1000);
  if (!Number.isSafeInteger(expectedPoints) || value.points.length !== expectedPoints) return null;
  let total = 0;
  let hasData = false;
  for (let index = 0; index < value.points.length; index += 1) {
    const point = value.points[index];
    const from = Date.parse(point?.from);
    const to = Date.parse(point?.to);
    if (!exactKeys(point, ['from', 'to', 'usage_request_count', 'reconciliation_attention_count'])
      || !Number.isSafeInteger(point.usage_request_count) || point.usage_request_count < 0
      || !Number.isSafeInteger(point.reconciliation_attention_count) || point.reconciliation_attention_count < 0
      || from !== Date.parse(window.from) + index * value.bucket_seconds * 1000
      || to !== from + value.bucket_seconds * 1000) return null;
    total += point.usage_request_count;
    hasData ||= point.usage_request_count > 0 || point.reconciliation_attention_count > 0;
  }
  return total === usageTotal ? { ...value, hasData } : null;
}

export function validateProvisioningPageOverview(value, expectedPermissionView = '') {
  const keys = { TOKEN_ONLY: ['schema_version', 'as_of', 'permission_view', 'provisioning'], TOKEN_AND_USAGE: ['schema_version', 'as_of', 'permission_view', 'provisioning', 'usage'] }[value?.permission_view];
  if (!keys || (expectedPermissionView && value.permission_view !== expectedPermissionView) || !exactKeys(value, keys) || value.schema_version !== '1' || !validTimestamp(value.as_of) || !exactKeys(value.provisioning, ['metrics'])) return null;
  const metrics = validatePageOverviewMetrics(value.provisioning.metrics, modelTokenPageOverviewSections.modelOperationsToken);
  if (!metrics) return null;
  if (value.permission_view === 'TOKEN_AND_USAGE') {
    if (!exactKeys(value.usage, ['window', 'metrics', 'trend'])) return null;
    const window = validateUsageWindow(value.usage.window, value.as_of);
    const usageMetrics = validatePageOverviewMetrics(value.usage.metrics, modelTokenPageOverviewSections.modelOperationsUsage);
    if (!window || !usageMetrics || !validateUsageTrend(value.usage.trend, window, usageMetrics.get('usage_request_count').value)) return null;
  }
  return { ...value, provisioning: { ...value.provisioning, metrics } };
}
export const pageOverviewInteraction = Object.freeze({ EXACT: 'EXACT', RELATED: 'RELATED', NON_INTERACTIVE: 'NON_INTERACTIVE' });
export function modelTokenPageOverviewDrilldown(section, key) {
  const mapping = {
    models: { published_model_count: { interaction: 'EXACT', source: 'models', query: { lifecycle_status: 'PUBLISHED' }, page: { model_tab: 'directory' } }, deprecated_model_count: { interaction: 'EXACT', source: 'models', query: { lifecycle_status: 'DEPRECATED' }, page: { model_tab: 'directory' } }, model_draft_count: { interaction: 'NON_INTERACTIVE' } },
    tokenResources: { stable_route_count: { interaction: 'NON_INTERACTIVE' }, healthy_route_count: { interaction: 'RELATED', source: 'tokenAccounts', query: { health_status: 'HEALTHY' } }, route_attention_count: { interaction: 'RELATED', source: 'tokenAccounts', query: {} } },
    modelOperations: { provisioning_pending_count: { interaction: 'EXACT', source: 'provisionings', query: { status: 'PENDING' } }, provisioning_attention_count: { interaction: 'RELATED', source: 'provisionings', query: {} } },
  };
  const target = mapping[section]?.[key];
  return target ? { section, ...target } : null;
}
