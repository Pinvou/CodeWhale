export const adminNavigationGroups = Object.freeze([
  { key: 'workbench', sections: ['overview'] },
  { key: 'identity', sections: ['users', 'roles', 'organizations', 'myorg'] },
  { key: 'finance', sections: ['billing', 'payments', 'products'] },
  { key: 'models', sections: ['tokens'] },
  { key: 'governance', sections: ['approvals', 'audit', 'system', 'settings'] },
]);

export function readAdminSection(locationLike = globalThis.location) {
  if (!locationLike) return '';
  return new URLSearchParams(String(locationLike.search || '')).get('section') || '';
}

const organizationTabs = new Set(['overview', 'members', 'units', 'lifecycle']);
const financeTabs = Object.freeze({
  billing: new Set(['wallets', 'receivables', 'adjustments', 'holds']),
  payments: new Set(['orders', 'refunds', 'lateReviews', 'anomalies', 'reconciliations', 'controls']),
  products: new Set(['catalog', 'prices']),
});
const financeDefaultTabs = Object.freeze({ billing: 'wallets', payments: 'orders', products: 'catalog' });
const financeQueryParameters = Object.freeze({
  receivables: Object.freeze({ wallet_id: 'receivable_wallet_id', status: 'receivable_status', limit: 'receivable_limit' }),
  orders: Object.freeze({ q: 'payment_q', status: 'payment_status', limit: 'payment_limit' }),
  lateReviews: Object.freeze({
    status: 'late_status',
    provider_id: 'late_provider_id',
    owner_id: 'late_owner_id',
    entered_from: 'late_entered_from',
    entered_to: 'late_entered_to',
    next_review_before: 'late_next_review_before',
    overdue: 'late_overdue',
    limit: 'late_limit',
  }),
  anomalies: Object.freeze({ anomaly_type: 'anomaly_type', status: 'anomaly_status', severity: 'anomaly_severity', owner_id: 'anomaly_owner_id', limit: 'anomaly_limit' }),
});
const financeQueryDefaults = Object.freeze({
  receivables: Object.freeze({ wallet_id: '', status: '', limit: '100' }),
  orders: Object.freeze({ q: '', status: '', limit: '100' }),
  lateReviews: Object.freeze({ status: '', provider_id: '', owner_id: '', entered_from: '', entered_to: '', next_review_before: '', overdue: false, limit: '100' }),
  anomalies: Object.freeze({ anomaly_type: '', status: 'OPEN', severity: '', owner_id: '', limit: '100' }),
});
const financeListLimits = new Set(['25', '50', '100', '200']);
const financeFilterValues = Object.freeze({
  receivables: Object.freeze({ status: new Set(['', 'OPEN', 'PARTIALLY_SETTLED', 'SETTLED', 'WRITTEN_OFF']) }),
  orders: Object.freeze({ status: new Set(['', 'CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW']) }),
  lateReviews: Object.freeze({ status: new Set(['', 'REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND', 'ARCHIVED_CONFIRMED_UNPAID', 'RESOLVED_PAID']) }),
  anomalies: Object.freeze({
    anomaly_type: new Set(['', 'CHARGEBACK', 'FORCED_REFUND', 'DUPLICATE_PAYMENT', 'LATE_PAYMENT_CONFLICT', 'PAYMENT_FACT_CONFLICT', 'REFUND_FAILURE', 'ILLEGAL_TRANSITION']),
    status: new Set(['', 'OPEN', 'RESOLVED']),
    severity: new Set(['', 'P0', 'P1', 'P2']),
  }),
});

export function readAdminFinanceTab(section, locationLike = globalThis.location) {
  const requestedTab = new URLSearchParams(String(locationLike?.search || '')).get('finance_tab') || '';
  return financeTabs[section]?.has(requestedTab) ? requestedTab : financeDefaultTabs[section] || '';
}

export function normalizeAdminFinanceQuery(tab, input = {}) {
  const defaults = financeQueryDefaults[tab] || {};
  const normalized = { ...defaults };
  for (const key of Object.keys(defaults)) {
    if (key === 'overdue') normalized[key] = input[key] === true || input[key] === 'true';
    else if (key === 'limit') normalized[key] = financeListLimits.has(String(input[key] || '')) ? String(input[key]) : defaults[key];
    else normalized[key] = String(input[key] || '').trim();
  }
  for (const [key, allowed] of Object.entries(financeFilterValues[tab] || {})) {
    if (!allowed.has(normalized[key])) normalized[key] = defaults[key];
  }
  if (tab === 'lateReviews') {
    for (const key of ['entered_from', 'entered_to', 'next_review_before']) {
      if (normalized[key] && (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(normalized[key]) || Number.isNaN(new Date(normalized[key]).getTime()))) normalized[key] = '';
    }
  }
  return normalized;
}

export function readAdminFinanceQuery(section, tab, locationLike = globalThis.location) {
  if (!financeTabs[section]?.has(tab) || !financeQueryParameters[tab]) return {};
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const raw = {};
  for (const [key, parameter] of Object.entries(financeQueryParameters[tab])) raw[key] = search.get(parameter) || '';
  return normalizeAdminFinanceQuery(tab, raw);
}

export function adminFinanceQueryUpdates(tab, input = {}) {
  if (!financeQueryParameters[tab]) return {};
  const query = normalizeAdminFinanceQuery(tab, input);
  return Object.fromEntries(Object.entries(financeQueryParameters[tab]).map(([key, parameter]) => [
    parameter,
    key === 'overdue' ? (query[key] ? 'true' : null) : query[key] || null,
  ]));
}

export function clearAdminFinanceQueryUpdates() {
  return Object.fromEntries(Object.values(financeQueryParameters).flatMap((parameters) => Object.values(parameters)).map((parameter) => [parameter, null]));
}

export function resolveAdminFinanceTab(requestedTab, currentTab, visibleTabs = []) {
  if (visibleTabs.includes(requestedTab)) return { tab: requestedTab, replace: false };
  const fallback = visibleTabs.includes(currentTab) ? currentTab : visibleTabs[0] || '';
  return { tab: fallback, replace: Boolean(fallback && fallback !== requestedTab) };
}

export function readAdminOrganizationRoute(locationLike = globalThis.location) {
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const organizationID = search.get('organization_id') || '';
  const requestedTab = search.get('organization_tab') || 'overview';
  return {
    organizationID,
    tab: organizationTabs.has(requestedTab) ? requestedTab : 'overview',
  };
}

export function readAdminOrganizationDirectory(locationLike = globalThis.location) {
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const requestedStatus = search.get('organization_status') || '';
  return {
    q: search.get('organization_q') || '',
    status: ['ACTIVE', 'DISABLED', 'DELETED'].includes(requestedStatus) ? requestedStatus : '',
    cursor: search.get('organization_cursor') || '',
  };
}

export function adminSectionURL(section, locationLike = globalThis.location, queryUpdates = {}) {
  const pathname = String(locationLike?.pathname || '/admin.html');
  const search = new URLSearchParams(String(locationLike?.search || ''));
  search.set('section', section);
  for (const [key, value] of Object.entries(queryUpdates || {})) {
    if (value === null || value === undefined || value === '') search.delete(key);
    else search.set(key, String(value));
  }
  const query = search.toString();
  return `${pathname}${query ? `?${query}` : ''}${String(locationLike?.hash || '')}`;
}

export function writeAdminSection(section, mode = 'push', historyLike = globalThis.history, locationLike = globalThis.location, queryUpdates = {}) {
  if (!historyLike || !section) return;
  const url = adminSectionURL(section, locationLike, queryUpdates);
  if (mode === 'replace') historyLike.replaceState({ section }, '', url);
  else historyLike.pushState({ section }, '', url);
}
