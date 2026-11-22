const definitions = Object.freeze({
  products: { prefix: 'product', defaultLimit: '25', fields: ['q', 'lifecycle_status', 'include_deleted'], statuses: ['DRAFT', 'ON_SALE', 'OFF_SALE', 'DELETED'] },
  orders: { prefix: 'order', defaultLimit: '25', fields: ['q', 'status', 'provider_id', 'channel', 'created_from', 'created_to'], statuses: ['CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW'] },
  wallets: { prefix: 'wallet', defaultLimit: '25', fields: ['q', 'status', 'owner_type'], statuses: ['ACTIVE', 'FROZEN', 'CLOSED'] },
  ledger: { prefix: 'ledger', fields: ['entry_type', 'business_reference', 'created_from', 'created_to'] },
  late: { prefix: 'late', defaultLimit: '25', fields: ['q', 'status', 'severity', 'owner_id', 'provider_id', 'created_from', 'created_to', 'entered_from', 'entered_to', 'next_review_before', 'overdue'], statuses: ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND', 'ARCHIVED_CONFIRMED_UNPAID', 'RESOLVED_PAID'] },
  anomalies: { prefix: 'anomaly', defaultLimit: '25', fields: ['q', 'status', 'anomaly_type', 'severity', 'owner_id', 'provider_id', 'created_from', 'created_to'], statuses: ['OPEN', 'RESOLVED'] },
  receivables: { prefix: 'receivable', fields: ['wallet_id', 'status', 'source_type', 'created_from', 'created_to'] },
  refunds: { prefix: 'refund', fields: ['q', 'status', 'provider_id', 'created_from', 'created_to'] },
  adjustments: { prefix: 'adjustment', fields: ['wallet_id', 'status', 'requested_by', 'created_from', 'created_to'] },
});
export function financeReadDefinition(kind) { return definitions[kind]; }
export function financeReadIdentity(kind, item) {
  if (kind === 'refunds') {
    if (!['NORMAL', 'FORCED'].includes(item.operation_type)) throw new Error('INVALID_FINANCE_ACK');
    return `${item.operation_type}:${item.operation_id}`;
  }
  return item[kind === 'receivables' ? 'receivable_id' : 'adjustment_id'];
}
export function readFinanceListRoute(kind, search) {
  const config = definitions[kind], params = new URLSearchParams(search), query = {};
  for (const field of [...config.fields, 'limit', 'cursor']) query[field] = params.get(`${config.prefix}_${field}`) || '';
  if (!['25', '50', '100', '200'].includes(query.limit)) query.limit = config.defaultLimit || '50';
  return { query, selectedID: params.get(`${config.prefix}_id`) || '' };
}
export function financeListRouteUpdates(kind, query, selectedID = null, referenceQuery = {}) {
  const config = definitions[kind];
  const rfc3339 = value => /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) && Number.isFinite(Date.parse(value));
  const wireValue = key => {
    const value = query[key];
    if (value && /_(from|to|before)$/.test(key)) {
      if (rfc3339(value)) return value;
      const date = new Date(value);
      if (Number.isFinite(date.getTime())) {
        const previous = referenceQuery[key];
        // Preserve the server's fractional precision when an unchanged local
        // date control is submitted. JavaScript Date only represents millis.
        if (rfc3339(previous)) {
          const parts = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2})(?::(\d{2})(?:\.(\d{1,3}))?)?$/.exec(value);
          const priorDate = new Date(previous), pad = number => String(number).padStart(2, '0');
          const priorLocal = `${priorDate.getFullYear()}-${pad(priorDate.getMonth() + 1)}-${pad(priorDate.getDate())}T${pad(priorDate.getHours())}:${pad(priorDate.getMinutes())}:${pad(priorDate.getSeconds())}.${String(priorDate.getMilliseconds()).padStart(3, '0')}`;
          // Compare displayed civil fields, since parsing a repeated DST hour
          // chooses the earlier offset even when the reference used the later.
          if (parts && `${parts[1]}:${parts[2] || '00'}.${(parts[3] || '').padEnd(3, '0')}` === priorLocal) return previous;
        }
        return date.toISOString();
      }
    }
    return value || null;
  };
  return Object.fromEntries([...config.fields, 'limit', 'cursor'].map(key => [`${config.prefix}_${key}`, wireValue(key)]).concat([[`${config.prefix}_id`, selectedID]]));
}
