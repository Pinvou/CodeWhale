const metricKeys = Object.freeze([
  'published_model_count', 'deprecated_model_count', 'model_draft_count',
  'stable_route_count', 'healthy_route_count', 'route_attention_count',
  'provisioning_pending_count', 'provisioning_attention_count', 'usage_request_count',
  'reconciliation_pending_count', 'reconciliation_stopped_count',
]);

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

export function validateCatalogDifference(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const keys = ['baseline_catalog_version', 'target_catalog_version', 'target_observed_version', 'added_count', 'modified_count', 'removed_count', 'total_count', 'truncated', 'items'];
  if (Object.keys(value).length !== keys.length || keys.some((key) => !Object.prototype.hasOwnProperty.call(value, key))) return null;
  const providerObservation = value.target_observed_version !== null;
  if (!Number.isSafeInteger(value.baseline_catalog_version) || value.baseline_catalog_version < 0
    || providerObservation === (value.target_catalog_version !== null)
    || (value.target_catalog_version !== null && (!Number.isSafeInteger(value.target_catalog_version) || value.target_catalog_version < 1))
    || (providerObservation && !/^[0-9a-f]{64}$/.test(value.target_observed_version))) return null;
  if (![value.added_count, value.modified_count, value.removed_count, value.total_count].every((item) => Number.isSafeInteger(item) && item >= 0) || value.total_count !== value.added_count + value.modified_count + value.removed_count || typeof value.truncated !== 'boolean' || !Array.isArray(value.items) || value.items.length > 20 || value.truncated !== (value.total_count > value.items.length)) return null;
  const visibleCounts = { ADDED: 0, MODIFIED: 0, REMOVED: 0 };
  const seenIDs = new Set();
  for (const item of value.items) {
    if (!item || Object.keys(item).sort().join(',') !== 'change_type,display_name,model_id' || !['ADDED', 'MODIFIED', 'REMOVED'].includes(item.change_type) || typeof item.display_name !== 'string' || item.display_name.length > 160 || (item.model_id !== null && !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(item.model_id))) return null;
    if (providerObservation ? item.change_type !== 'ADDED' && item.model_id === null : item.model_id === null) return null;
    if (item.model_id !== null && seenIDs.has(item.model_id.toLowerCase())) return null;
    if (item.model_id !== null) seenIDs.add(item.model_id.toLowerCase());
    visibleCounts[item.change_type] += 1;
  }
  if (visibleCounts.ADDED > value.added_count || visibleCounts.MODIFIED > value.modified_count || visibleCounts.REMOVED > value.removed_count) return null;
  if (!value.truncated && (visibleCounts.ADDED !== value.added_count || visibleCounts.MODIFIED !== value.modified_count || visibleCounts.REMOVED !== value.removed_count)) return null;
  return value;
}
