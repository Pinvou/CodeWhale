const terminalExecutionStatuses = new Set(['SUCCEEDED', 'FAILED', 'CANCELLED_BEFORE_DISPATCH']);

function numeric(value) {
  if (value == null || value === '') return null;
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? number : null;
}

function recordKey(record, index) {
  return record?.logical_turn_id || record?.usage_request_id || `legacy-usage-${index}`;
}

function isUsageTerminal(record) {
  return record?.delivery_status === 'USER_STOPPED' || terminalExecutionStatuses.has(record?.execution_status);
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
      };
      groups.set(key, summary);
    }

    summary.usage_request_count += 1;
    if (record?.usage_request_id) summary.usage_request_ids.push(record.usage_request_id);
    if (!summary.cancel_usage_request_id && !isUsageTerminal(record) && record?.usage_request_id) {
      summary.cancel_usage_request_id = record.usage_request_id;
      for (const field of ['display_status', 'execution_status', 'delivery_status', 'settlement_status']) {
        summary[field] = record[field];
      }
    }

    const actual = numeric(record?.actual_microcredits);
    if (actual == null && record?.display_status !== 'RELEASED') summary._actualComplete = false;
    else summary.actual_microcredits += actual || 0;

    for (const field of ['input_tokens', 'output_tokens', 'cached_input_tokens']) {
      const amount = numeric(record?.[field]);
      if (amount == null) summary._tokensComplete = false;
      else summary[field] += amount;
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
    return summary;
  });
}
