const cancellableExecutionStatuses = new Set([
  'RECEIVED',
  'HOLD_RESERVING',
  'HOLD_RESERVED',
  'DISPATCH_INTENT_PERSISTED',
  'UPSTREAM_DISPATCHING',
  'STREAMING',
]);

function numeric(value) {
  if (value == null || value === '') return null;
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? number : null;
}

function recordKey(record, index) {
  if (record?.logical_turn_id) return `turn:${record.logical_turn_id}`;
  if (record?.usage_request_id) return `request:${record.usage_request_id}`;
  return `legacy-usage-${index}`;
}

function isUsageCancellable(record) {
  return record?.delivery_status !== 'USER_STOPPED'
    && !['COMMITTED', 'RELEASED', 'REVIEW'].includes(record?.status)
    && !['SETTLED', 'RELEASED', 'REVIEW'].includes(record?.settlement_status)
    && cancellableExecutionStatuses.has(record?.execution_status);
}

function isNoCharge(record) {
  if (record?.settlement_status === 'RELEASED') return true;
  if (record?.settlement_status) return false;
  return record?.display_status === 'RELEASED' || record?.display_status === 'CANCELLED_BEFORE_DISPATCH';
}

function missingTokensMeanZero(record) {
  return record?.display_status === 'CANCELLED_BEFORE_DISPATCH';
}

function chargedMicrocredits(record) {
  if (Object.prototype.hasOwnProperty.call(record || {}, 'charged_microcredits')) return numeric(record?.charged_microcredits);
  // Rolling-deploy compatibility with the previous API shape. The fallback
  // preserves charge semantics and never treats released Provider cost as a debit.
  if (record?.settlement_status === 'SETTLED') return numeric(record?.actual_microcredits);
  if (isNoCharge(record)) return 0;
  if (!record?.settlement_status && record?.display_status === 'SETTLED') return numeric(record?.actual_microcredits);
  return null;
}

function aggregateDisplayStatus(records) {
  if (records.some((record) => record?.display_status === 'MANUAL_REVIEW' || record?.execution_status === 'REVIEW' || record?.settlement_status === 'REVIEW')) return 'MANUAL_REVIEW';
  if (records.some((record) => isUsageCancellable(record) || record?.display_status === 'IN_PROGRESS')) return 'IN_PROGRESS';
  if (records.some((record) => record?.display_status === 'DISPLAY_STOPPED_EXECUTION_CONTINUES')) return 'DISPLAY_STOPPED_EXECUTION_CONTINUES';
  if (records.some((record) => record?.display_status === 'SETTLEMENT_PENDING' || record?.execution_status === 'SETTLEMENT_PENDING' || record?.settlement_status === 'USAGE_PENDING')) return 'SETTLEMENT_PENDING';
  if (records.some((record) => record?.display_status === 'SETTLED' || record?.settlement_status === 'SETTLED')) return 'SETTLED';
  if (records.every((record) => record?.display_status === 'CANCELLED_BEFORE_DISPATCH')) return 'CANCELLED_BEFORE_DISPATCH';
  if (records.every(isNoCharge)) return 'RELEASED';
  return 'MANUAL_REVIEW';
}

export function summarizeUsageRecords(records = []) {
  const groups = new Map();
  records.forEach((record, index) => {
    const key = recordKey(record, index);
    let summary = groups.get(key);
    if (!summary) {
      summary = {
        ...record,
        logical_turn_id: record?.logical_turn_id || '',
        usage_request_count: 0,
        usage_request_ids: [],
        cancel_usage_request_id: '',
        actual_microcredits: 0,
        charged_microcredits: 0,
        input_tokens: 0,
        output_tokens: 0,
        cached_input_tokens: 0,
        _actualComplete: true,
        _chargedComplete: true,
        _tokensComplete: true,
        _records: [],
      };
      groups.set(key, summary);
    }

    const requestCount = Math.max(1, Number(record?.usage_request_count) || 1);
    summary.usage_request_count += requestCount;
    summary._records.push(record);
    if (Array.isArray(record?.usage_request_ids)) summary.usage_request_ids.push(...record.usage_request_ids);
    else if (record?.usage_request_id) summary.usage_request_ids.push(record.usage_request_id);
    const cancellableID = record?.cancel_usage_request_id || (isUsageCancellable(record) ? record?.usage_request_id : '');
    if (!summary.cancel_usage_request_id && cancellableID) {
      summary.cancel_usage_request_id = cancellableID;
    }
    const actual = numeric(record?.actual_microcredits);
    if (actual == null) summary._actualComplete = false;
    else summary.actual_microcredits += actual || 0;

    const charged = chargedMicrocredits(record);
    if (charged == null) summary._chargedComplete = false;
    else summary.charged_microcredits += charged || 0;

    for (const field of ['input_tokens', 'output_tokens', 'cached_input_tokens']) {
      const amount = numeric(record?.[field]);
      if (amount == null && !missingTokensMeanZero(record)) summary._tokensComplete = false;
      else summary[field] += amount || 0;
    }

    const startedAt = Date.parse(record?.started_at || '');
    const summaryStartedAt = Date.parse(summary.started_at || '');
    if (Number.isFinite(startedAt) && (!Number.isFinite(summaryStartedAt) || startedAt < summaryStartedAt)) {
      summary.started_at = record.started_at;
    }
    const completedAt = Date.parse(record?.completed_at || '');
    const summaryCompletedAt = Date.parse(summary.completed_at || '');
    if (Number.isFinite(completedAt) && (!Number.isFinite(summaryCompletedAt) || completedAt > summaryCompletedAt)) {
      summary.completed_at = record.completed_at;
    }
  });

  return [...groups.values()].map((summary) => {
    summary.display_status = aggregateDisplayStatus(summary._records);
    if (!summary._actualComplete) summary.actual_microcredits = null;
    if (!summary._chargedComplete) summary.charged_microcredits = null;
    if (summary.cancel_usage_request_id) summary.completed_at = '';
    if (summary.usage_request_count > 1) {
      summary.execution_status = '';
      summary.delivery_status = '';
      summary.settlement_status = '';
    }
    if (!summary._tokensComplete) {
      summary.input_tokens = null;
      summary.output_tokens = null;
      summary.cached_input_tokens = null;
    }
    delete summary._actualComplete;
    delete summary._chargedComplete;
    delete summary._tokensComplete;
    delete summary._records;
    return summary;
  });
}
