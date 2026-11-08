const metricKeys = Object.freeze([
  'published_model_count', 'deprecated_model_count', 'model_draft_count',
  'stable_route_count', 'healthy_route_count', 'route_attention_count',
  'provisioning_pending_count', 'provisioning_attention_count', 'usage_request_count',
  'reconciliation_pending_count', 'reconciliation_stopped_count',
]);

const modelTokenPageOverviewSections = Object.freeze({
  models: Object.freeze(['published_model_count', 'deprecated_model_count', 'model_draft_count']),
  tokenResources: Object.freeze(['stable_route_count', 'healthy_route_count', 'route_attention_count']),
  modelOperationsToken: Object.freeze(['provisioning_pending_count', 'provisioning_attention_count']),
  modelOperationsUsage: Object.freeze(['usage_request_count', 'reconciliation_pending_count', 'reconciliation_stopped_count']),
});

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

export const pageOverviewInteraction = Object.freeze({
  EXACT: 'EXACT',
  RELATED: 'RELATED',
  NON_INTERACTIVE: 'NON_INTERACTIVE',
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

function validateUsageWindow(value, asOf) {
  if (!exactKeys(value, ['from', 'to', 'timezone']) || value.timezone !== 'UTC'
    || !validTimestamp(value.from) || !validTimestamp(value.to)
    || Date.parse(value.from) >= Date.parse(value.to) || Date.parse(value.to) !== Date.parse(asOf)) return null;
  const duration = Date.parse(value.to) - Date.parse(value.from);
  if (![24 * 3600e3, 7 * 86400e3, 30 * 86400e3].includes(duration)) return null;
  return value;
}

function validateUsageTrend(value, window, usageTotal) {
  if (!exactKeys(value, ['bucket_seconds', 'points']) || ![3600, 86400].includes(value.bucket_seconds) || !Array.isArray(value.points)) return null;
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

export function validateModelOperationsPageOverview(value, expectedPermissionView = '') {
  const views = {
    TOKEN_ONLY: ['schema_version', 'as_of', 'permission_view', 'provisioning'],
    USAGE_ONLY: ['schema_version', 'as_of', 'permission_view', 'usage'],
    TOKEN_AND_USAGE: ['schema_version', 'as_of', 'permission_view', 'provisioning', 'usage'],
  };
  const keys = views[value?.permission_view];
  if (!keys || (expectedPermissionView && value.permission_view !== expectedPermissionView)
    || !exactKeys(value, keys) || value.schema_version !== '1' || !validTimestamp(value.as_of)) return null;
  let provisioning = null;
  if (value.permission_view !== 'USAGE_ONLY') {
    if (!exactKeys(value.provisioning, ['metrics'])) return null;
    const metrics = validatePageOverviewMetrics(value.provisioning.metrics, modelTokenPageOverviewSections.modelOperationsToken);
    if (!metrics) return null;
    provisioning = { ...value.provisioning, metrics };
  }
  let usage = null;
  if (value.permission_view !== 'TOKEN_ONLY') {
    if (!exactKeys(value.usage, ['window', 'metrics', 'trend'])) return null;
    const window = validateUsageWindow(value.usage.window, value.as_of);
    const metrics = validatePageOverviewMetrics(value.usage.metrics, modelTokenPageOverviewSections.modelOperationsUsage);
    const trend = window && metrics ? validateUsageTrend(value.usage.trend, window, metrics.get('usage_request_count').value) : null;
    if (!window || !metrics || !trend) return null;
    usage = { ...value.usage, window, metrics, trend };
  }
  return { ...value, ...(provisioning ? { provisioning } : {}), ...(usage ? { usage } : {}) };
}

export function modelTokenPageOverviewPermissionView(permissions = []) {
  const granted = new Set(permissions);
  const token = granted.has('platform.tokens.read');
  const usage = granted.has('platform.usage.read');
  return token && usage ? 'TOKEN_AND_USAGE' : token ? 'TOKEN_ONLY' : usage ? 'USAGE_ONLY' : '';
}

export function modelTokenPageOverviewDrilldown(section, key, window = null, bucket = null) {
  const mapping = {
    models: {
      published_model_count: { interaction: pageOverviewInteraction.EXACT, source: 'models', query: { lifecycle_status: 'PUBLISHED' }, page: { model_tab: 'directory' } },
      deprecated_model_count: { interaction: pageOverviewInteraction.EXACT, source: 'models', query: { lifecycle_status: 'DEPRECATED' }, page: { model_tab: 'directory' } },
      model_draft_count: { interaction: pageOverviewInteraction.NON_INTERACTIVE },
    },
    tokenResources: {
      stable_route_count: { interaction: pageOverviewInteraction.NON_INTERACTIVE },
      healthy_route_count: { interaction: pageOverviewInteraction.RELATED, source: 'tokenAccounts', query: { health_status: 'HEALTHY' } },
      route_attention_count: { interaction: pageOverviewInteraction.RELATED, source: 'tokenAccounts', query: {} },
    },
    modelOperations: {
      provisioning_pending_count: { interaction: pageOverviewInteraction.EXACT, source: 'provisionings', query: { status: 'PENDING' }, page: { operations_tab: 'provisionings' } },
      provisioning_attention_count: { interaction: pageOverviewInteraction.RELATED, source: 'provisionings', query: {}, page: { operations_tab: 'provisionings' } },
      usage_request_count: { interaction: pageOverviewInteraction.EXACT, source: 'usage', query: { started_from: window?.from, started_to: window?.to }, page: { operations_tab: 'usage' } },
      reconciliation_pending_count: { interaction: pageOverviewInteraction.RELATED, source: 'reconciliation', query: {}, page: { operations_tab: 'reconciliation' } },
      reconciliation_stopped_count: { interaction: pageOverviewInteraction.RELATED, source: 'reconciliation', query: {}, page: { operations_tab: 'reconciliation' } },
      usage_trend: { interaction: pageOverviewInteraction.EXACT, source: 'usage', query: { started_from: bucket?.from || window?.from, started_to: bucket?.to || window?.to }, page: { operations_tab: 'usage' } },
      reconciliation_trend: { interaction: pageOverviewInteraction.RELATED, source: 'reconciliation', query: {}, page: { operations_tab: 'reconciliation' } },
    },
  };
  const target = mapping[section]?.[key];
  if (!target) return null;
  return { section, ...target };
}

export function modelOperationsWindow(value) {
  return value === '30d' || value === '7d' ? value : '24h';
}

export function validateModelOperationsOverview(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const top = ['as_of', 'window', 'metrics', 'trend'];
  if (Object.keys(value).length !== top.length || top.some((key) => !Object.prototype.hasOwnProperty.call(value, key))) return null;
  if (Number.isNaN(Date.parse(value.as_of)) || !value.window || Array.isArray(value.window) || Object.keys(value.window).sort().join(',') !== 'from,timezone,to' || value.window.timezone !== 'UTC' || Number.isNaN(Date.parse(value.window.from)) || Number.isNaN(Date.parse(value.window.to)) || Date.parse(value.window.from) >= Date.parse(value.window.to)) return null;
  if (!Array.isArray(value.metrics) || value.metrics.length !== metricKeys.length) return null;
  const metrics = new Map();
  for (const metric of value.metrics) {
    const keys = ['key', 'value', 'unit', 'aggregation', 'included_statuses', ...(metric.denominator_key ? ['denominator_key'] : [])];
    if (!metric || Object.keys(metric).length !== keys.length || keys.some((key) => !Object.prototype.hasOwnProperty.call(metric, key)) || !metricKeys.includes(metric.key) || metrics.has(metric.key) || !Number.isSafeInteger(metric.value) || metric.value < 0 || metric.unit !== 'COUNT' || !['POINT_IN_TIME', 'WINDOW'].includes(metric.aggregation) || !Array.isArray(metric.included_statuses) || metric.included_statuses.length < 1) return null;
    if (metric.denominator_key && metric.denominator_key !== 'stable_route_count') return null;
    metrics.set(metric.key, metric);
  }
  if (metricKeys.some((key) => !metrics.has(key))) return null;
  const trend = value.trend;
  if (!trend || Array.isArray(trend) || Object.keys(trend).sort().join(',') !== 'bucket_seconds,points' || ![3600, 86400].includes(trend.bucket_seconds) || !Array.isArray(trend.points)) return null;
  const fromMillis = Date.parse(value.window.from);
  const toMillis = Date.parse(value.window.to);
  if (Date.parse(value.as_of) !== toMillis) return null;
  const durationSeconds = (toMillis - fromMillis) / 1000;
  const expected = durationSeconds === 24 * 3600 ? { bucketSeconds: 3600, points: 24 }
    : durationSeconds === 7 * 86400 ? { bucketSeconds: 86400, points: 7 }
      : durationSeconds === 30 * 86400 ? { bucketSeconds: 86400, points: 30 } : null;
  if (!expected || trend.bucket_seconds !== expected.bucketSeconds || trend.points.length !== expected.points) return null;
  let usageTotal = 0;
  let trendHasData = false;
  for (let index = 0; index < trend.points.length; index += 1) {
    const point = trend.points[index];
    const pointFrom = Date.parse(point?.from);
    const pointTo = Date.parse(point?.to);
    if (!point || Object.keys(point).sort().join(',') !== 'from,reconciliation_attention_count,to,usage_request_count' || Number.isNaN(pointFrom) || Number.isNaN(pointTo) || pointFrom !== fromMillis + index * trend.bucket_seconds * 1000 || pointTo !== fromMillis + (index + 1) * trend.bucket_seconds * 1000 || !Number.isSafeInteger(point.usage_request_count) || point.usage_request_count < 0 || !Number.isSafeInteger(point.reconciliation_attention_count) || point.reconciliation_attention_count < 0) return null;
    usageTotal += point.usage_request_count;
    trendHasData ||= point.usage_request_count > 0 || point.reconciliation_attention_count > 0;
  }
  if (usageTotal !== metrics.get('usage_request_count').value) return null;
  return { ...value, metrics, trendHasData };
}

export function modelOperationsOverviewDrilldown(key, window) {
  const range = { started_from: window.from, started_to: window.to, cursor: null };
  return {
    published_model_count: { section: 'models', query: { model_tab: 'directory', model_lifecycle_status: 'PUBLISHED', model_cursor: null }, exact: true },
    deprecated_model_count: { section: 'models', query: { model_tab: 'directory', model_lifecycle_status: 'DEPRECATED', model_cursor: null }, exact: true },
    model_draft_count: { section: 'models', query: { model_tab: 'directory', model_cursor: null }, exact: false },
    stable_route_count: { section: 'tokenResources', query: { token_cursor: null }, exact: false },
    healthy_route_count: { section: 'tokenResources', query: { token_health_status: 'HEALTHY', token_cursor: null }, exact: false },
    route_attention_count: { section: 'tokenResources', query: { token_health_status: 'UNHEALTHY', token_cursor: null }, exact: false },
    provisioning_pending_count: { section: 'modelOperations', query: { operations_tab: 'provisionings', provisioning_status: 'PENDING', provisioning_cursor: null }, exact: true },
    provisioning_attention_count: { section: 'modelOperations', query: { operations_tab: 'provisionings', provisioning_cursor: null }, exact: false },
    usage_request_count: { section: 'modelOperations', query: { operations_tab: 'usage', usage_started_from: range.started_from, usage_started_to: range.started_to, usage_cursor: null }, exact: true },
    reconciliation_pending_count: { section: 'modelOperations', query: { operations_tab: 'reconciliation', reconciliation_status: 'OPEN', reconciliation_cursor: null }, exact: false },
    reconciliation_stopped_count: { section: 'modelOperations', query: { operations_tab: 'reconciliation', reconciliation_cursor: null }, exact: false },
  }[key] || null;
}

export function readAdminModelOperationsWindow(locationLike = globalThis.location) {
  const value = new URLSearchParams(String(locationLike?.search || '')).get('model_operations_window') || '24h';
  return modelOperationsWindow(value);
}

export function usageOverviewRoute(target, queryUpdates) {
  if (!target) return null;
  return {
    section: target.section,
    query: { model_id: null, token_account_id: null, usage_request_id: null,
      usage_evidence_cursor: null, usage_evidence_type: null,
      ...queryUpdates, ...target.page },
  };
}
