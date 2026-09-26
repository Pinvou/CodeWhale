const cancellableExecutionStatuses = new Set([
  'RECEIVED',
  'HOLD_RESERVING',
  'HOLD_RESERVED',
  'DISPATCH_INTENT_PERSISTED',
  'UPSTREAM_DISPATCHING',
  'STREAMING',
]);

const unsettledDisplayStatuses = new Set([
  'IN_PROGRESS',
  'SETTLEMENT_PENDING',
  'MANUAL_REVIEW',
  'DISPLAY_STOPPED_EXECUTION_CONTINUES',
]);

function numeric(value) {
  if (value == null || value === '') return null;
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? number : null;
}

function recordKey(record, index) {
  return record?.logical_turn_id || record?.usage_request_id || `legacy-usage-${index}`;
}

function isUsageCancellable(record) {
  return record?.delivery_status !== 'USER_STOPPED' && cancellableExecutionStatuses.has(record?.execution_status);
}

function statusPriority(record) {
  if (isUsageCancellable(record)) return 6;
  if (record?.display_status === 'MANUAL_REVIEW' || record?.execution_status === 'REVIEW' || record?.settlement_status === 'REVIEW') return 5;
  if (record?.display_status === 'DISPLAY_STOPPED_EXECUTION_CONTINUES') return 4;
  if (unsettledDisplayStatuses.has(record?.display_status) || record?.execution_status === 'SETTLEMENT_PENDING' || record?.settlement_status === 'USAGE_PENDING') return 3;
  if (record?.display_status === 'SETTLED' || record?.settlement_status === 'SETTLED') return 2;
  return 1;
}

function isNoCharge(record) {
  if (record?.settlement_status === 'RELEASED') return true;
  if (record?.settlement_status) return false;
  return record?.display_status === 'RELEASED' || record?.display_status === 'CANCELLED_BEFORE_DISPATCH';
}

function settledMicrocredits(record) {
  if (record?.settlement_status === 'SETTLED') return numeric(record?.actual_microcredits);
  if (isNoCharge(record)) return 0;
  if (record?.settlement_status) return null;
  if (record?.display_status === 'SETTLED') return numeric(record?.actual_microcredits);
  return null;
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
        input_tokens: 0,
        output_tokens: 0,
        cached_input_tokens: 0,
        _actualComplete: true,
        _tokensComplete: true,
        _statusPriority: 0,
      };
      groups.set(key, summary);
    }

    summary.usage_request_count += 1;
    if (record?.usage_request_id) summary.usage_request_ids.push(record.usage_request_id);
    if (!summary.cancel_usage_request_id && isUsageCancellable(record) && record?.usage_request_id) {
      summary.cancel_usage_request_id = record.usage_request_id;
    }
    const priority = statusPriority(record);
    if (priority > summary._statusPriority) {
      summary._statusPriority = priority;
      for (const field of ['display_status', 'execution_status', 'delivery_status', 'settlement_status']) {
        summary[field] = record[field];
      }
    }

    const actual = settledMicrocredits(record);
    if (actual == null) summary._actualComplete = false;
    else summary.actual_microcredits += actual || 0;

    for (const field of ['input_tokens', 'output_tokens', 'cached_input_tokens']) {
      const amount = numeric(record?.[field]);
      if (amount == null && !isNoCharge(record)) summary._tokensComplete = false;
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
    if (!summary._actualComplete) summary.actual_microcredits = null;
    if (summary.cancel_usage_request_id) summary.completed_at = '';
    if (!summary._tokensComplete) {
      summary.input_tokens = null;
      summary.output_tokens = null;
      summary.cached_input_tokens = null;
    }
    delete summary._actualComplete;
    delete summary._tokensComplete;
    delete summary._statusPriority;
    return summary;
  });
}
