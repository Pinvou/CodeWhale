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
  wallets: Object.freeze({ owner_type: 'wallet_owner_type', status: 'wallet_status', q: 'wallet_q', limit: 'wallet_limit', cursor: 'wallet_cursor' }),
  receivables: Object.freeze({ wallet_id: 'receivable_wallet_id', status: 'receivable_status', source_type: 'receivable_source_type', created_from: 'receivable_created_from', created_to: 'receivable_created_to', limit: 'receivable_limit', cursor: 'receivable_cursor' }),
  adjustments: Object.freeze({ wallet_id: 'adjustment_wallet_id', status: 'adjustment_status', requested_by: 'adjustment_requested_by', created_from: 'adjustment_created_from', created_to: 'adjustment_created_to', limit: 'adjustment_limit', cursor: 'adjustment_cursor' }),
  holds: Object.freeze({ wallet_id: 'hold_wallet_id', status: 'hold_status', model_id: 'hold_model_id', created_from: 'hold_created_from', created_to: 'hold_created_to', limit: 'hold_limit', cursor: 'hold_cursor' }),
  orders: Object.freeze({ q: 'payment_q', status: 'payment_status', provider_id: 'payment_provider_id', channel: 'payment_channel', created_from: 'payment_created_from', created_to: 'payment_created_to', limit: 'payment_limit', cursor: 'payment_cursor' }),
  refunds: Object.freeze({ q: 'refund_q', status: 'refund_status', provider_id: 'refund_provider_id', created_from: 'refund_created_from', created_to: 'refund_created_to', limit: 'refund_limit', cursor: 'refund_cursor' }),
  lateReviews: Object.freeze({
    q: 'late_q',
    status: 'late_status',
    severity: 'late_severity',
    provider_id: 'late_provider_id',
    owner_id: 'late_owner_id',
    created_from: 'late_created_from',
    created_to: 'late_created_to',
    entered_from: 'late_entered_from',
    entered_to: 'late_entered_to',
    next_review_before: 'late_next_review_before',
    overdue: 'late_overdue',
    limit: 'late_limit',
    cursor: 'late_cursor',
  }),
  anomalies: Object.freeze({ q: 'anomaly_q', anomaly_type: 'anomaly_type', status: 'anomaly_status', severity: 'anomaly_severity', owner_id: 'anomaly_owner_id', provider_id: 'anomaly_provider_id', created_from: 'anomaly_created_from', created_to: 'anomaly_created_to', limit: 'anomaly_limit', cursor: 'anomaly_cursor' }),
  reconciliations: Object.freeze({ provider_id: 'reconciliation_provider_id', channel: 'reconciliation_channel', status: 'reconciliation_status', business_date_from: 'reconciliation_business_date_from', business_date_to: 'reconciliation_business_date_to', limit: 'reconciliation_limit', cursor: 'reconciliation_cursor' }),
});
const financeQueryDefaults = Object.freeze({
  wallets: Object.freeze({ owner_type: '', status: '', q: '', limit: '50', cursor: '' }),
  receivables: Object.freeze({ wallet_id: '', status: '', source_type: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  adjustments: Object.freeze({ wallet_id: '', status: '', requested_by: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  holds: Object.freeze({ wallet_id: '', status: '', model_id: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  orders: Object.freeze({ q: '', status: '', provider_id: '', channel: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  refunds: Object.freeze({ q: '', status: '', provider_id: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  lateReviews: Object.freeze({ q: '', status: '', severity: '', provider_id: '', owner_id: '', created_from: '', created_to: '', entered_from: '', entered_to: '', next_review_before: '', overdue: false, limit: '50', cursor: '' }),
  anomalies: Object.freeze({ q: '', anomaly_type: '', status: 'OPEN', severity: '', owner_id: '', provider_id: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  reconciliations: Object.freeze({ provider_id: '', channel: '', status: '', business_date_from: '', business_date_to: '', limit: '50', cursor: '' }),
});
const financeDetailQueryParameters = Object.freeze({
  ledger: Object.freeze({ entry_type: 'ledger_entry_type', business_reference: 'ledger_business_reference', created_from: 'ledger_created_from', created_to: 'ledger_created_to', limit: 'ledger_limit', cursor: 'ledger_cursor' }),
  reconciliationCases: Object.freeze({ status: 'case_status', severity: 'case_severity', owner_id: 'case_owner_id', case_type: 'case_type', created_from: 'case_created_from', created_to: 'case_created_to', limit: 'case_limit', cursor: 'case_cursor' }),
});
const financeDetailQueryDefaults = Object.freeze({
  ledger: Object.freeze({ entry_type: '', business_reference: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  reconciliationCases: Object.freeze({ status: '', severity: '', owner_id: '', case_type: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
});
const financeListLimits = new Set(['25', '50', '100', '200']);
const financeFilterValues = Object.freeze({
  wallets: Object.freeze({ owner_type: new Set(['', 'PERSONAL', 'ORGANIZATION', 'ORG_UNIT']), status: new Set(['', 'ACTIVE', 'FROZEN', 'CLOSED']) }),
  receivables: Object.freeze({ status: new Set(['', 'OPEN', 'PARTIALLY_SETTLED', 'SETTLED', 'WRITTEN_OFF']) }),
  adjustments: Object.freeze({ status: new Set(['', 'PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'POSTED']) }),
  holds: Object.freeze({ status: new Set(['', 'ACTIVE', 'COMMITTED', 'RELEASED', 'EXPIRED', 'REVIEW']) }),
  orders: Object.freeze({ status: new Set(['', 'CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW']) }),
  refunds: Object.freeze({ status: new Set(['', 'REQUESTED', 'SUCCEEDED', 'FAILED', 'MANUAL_REVIEW', 'REVIEW_REQUIRED', 'RESOLVED']) }),
  lateReviews: Object.freeze({ status: new Set(['', 'REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND', 'ARCHIVED_CONFIRMED_UNPAID', 'RESOLVED_PAID']), severity: new Set(['', 'P0', 'P1', 'P2']) }),
  anomalies: Object.freeze({
    anomaly_type: new Set(['', 'CHARGEBACK', 'FORCED_REFUND', 'DUPLICATE_PAYMENT', 'LATE_PAYMENT_CONFLICT', 'PAYMENT_FACT_CONFLICT', 'REFUND_FAILURE', 'ILLEGAL_TRANSITION']),
    status: new Set(['', 'OPEN', 'RESOLVED']),
    severity: new Set(['', 'P0', 'P1', 'P2']),
  }),
  reconciliations: Object.freeze({ status: new Set(['', 'RUNNING', 'COMPLETED', 'FAILED']) }),
});
const financeDetailFilterValues = Object.freeze({
  ledger: Object.freeze({
    entry_type: new Set(['', 'RECHARGE', 'USAGE', 'REFUND', 'REVERSAL', 'ADJUSTMENT', 'EXPIRY', 'RECEIVABLE', 'CHARGEBACK']),
  }),
  reconciliationCases: Object.freeze({
    status: new Set(['', 'OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED']),
    severity: new Set(['', 'P0', 'P1', 'P2']),
    case_type: new Set(['', 'PROVIDER_PAID_INTERNAL_MISSING', 'INTERNAL_PAID_PROVIDER_MISSING', 'AMOUNT_MISMATCH', 'DUPLICATE_TRANSACTION', 'PROVIDER_REFUND_INTERNAL_MISSING', 'INTERNAL_REFUND_PROVIDER_MISSING', 'REFUND_AMOUNT_MISMATCH', 'PROVIDER_CHARGEBACK_INTERNAL_MISSING', 'INTERNAL_CHARGEBACK_PROVIDER_MISSING', 'PAYMENT_LEDGER_MISSING', 'REFUND_LEDGER_MISSING', 'CHARGEBACK_LEDGER_MISSING', 'UNSUPPORTED_PROVIDER_ENTRY']),
  }),
});

export const financePaginatedTabs = Object.freeze(Object.keys(financeQueryParameters));

export function isAdminFinancePaginatedTab(tab) {
  return Object.hasOwn(financeQueryParameters, tab);
}

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
  for (const key of ['created_from', 'created_to', 'entered_from', 'entered_to', 'next_review_before']) {
    if (Object.hasOwn(normalized, key)) {
      if (normalized[key] && (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(normalized[key]) || Number.isNaN(new Date(normalized[key]).getTime()))) normalized[key] = '';
    }
  }
  for (const key of ['business_date_from', 'business_date_to']) {
    if (Object.hasOwn(normalized, key) && normalized[key] && !/^\d{4}-\d{2}-\d{2}$/.test(normalized[key])) normalized[key] = '';
  }
  return normalized;
}

export function normalizeAdminFinanceDetailQuery(type, input = {}) {
  const defaults = financeDetailQueryDefaults[type] || {};
  const normalized = { ...defaults };
  for (const key of Object.keys(defaults)) {
    if (key === 'limit') normalized[key] = financeListLimits.has(String(input[key] || '')) ? String(input[key]) : defaults[key];
    else normalized[key] = String(input[key] || '').trim();
  }
  for (const [key, allowed] of Object.entries(financeDetailFilterValues[type] || {})) {
    if (!allowed.has(normalized[key])) normalized[key] = defaults[key];
  }
  for (const key of ['created_from', 'created_to']) {
    if (normalized[key] && (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(normalized[key]) || Number.isNaN(new Date(normalized[key]).getTime()))) normalized[key] = '';
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

export function readAdminFinanceDetail(section, tab, locationLike = globalThis.location) {
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const type = search.get('finance_detail') || '';
  const id = search.get('finance_detail_id') || '';
  const valid = (section === 'billing' && tab === 'wallets' && type === 'ledger')
    || (section === 'payments' && tab === 'reconciliations' && type === 'reconciliationCases');
  if (!valid || !id || !financeDetailQueryParameters[type]) return { type: '', id: '', query: {} };
  const raw = {};
  for (const [key, parameter] of Object.entries(financeDetailQueryParameters[type])) raw[key] = search.get(parameter) || '';
  return { type, id, query: normalizeAdminFinanceDetailQuery(type, raw) };
}

export function adminFinanceDetailUpdates(type, id, input = {}) {
  const updates = clearAdminFinanceDetailUpdates();
  if (!financeDetailQueryParameters[type] || !id) return updates;
  const query = normalizeAdminFinanceDetailQuery(type, input);
  updates.finance_detail = type;
  updates.finance_detail_id = id;
  for (const [key, parameter] of Object.entries(financeDetailQueryParameters[type])) updates[parameter] = query[key] || null;
  return updates;
}

export function clearAdminFinanceDetailUpdates() {
  return {
    finance_detail: null,
    finance_detail_id: null,
    ...Object.fromEntries(Object.values(financeDetailQueryParameters).flatMap((parameters) => Object.values(parameters)).map((parameter) => [parameter, null])),
  };
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
