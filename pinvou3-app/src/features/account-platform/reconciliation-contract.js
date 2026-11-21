import { encodeFinanceJSON, integerMoney, parseFinanceJSON, requireFinanceID } from './finance-contract.js';

export const RECONCILIATION_ACTIONS = ['CLAIM', 'ASSIGN', 'SET_REPAIR_ACTION', 'RESOLVE', 'IGNORE', 'REOPEN'];
export function reconciliationCaseLabel(caseType, copy) {
  return copy.caseTypes?.[caseType] || caseType;
}
export function requireReconciliationDate(value) {
  const date = new Date(`${value}T00:00:00Z`);
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value) || !Number.isFinite(date.getTime()) || date.toISOString().slice(0, 10) !== value) throw new Error('INVALID_RECONCILIATION_INPUT');
  return value;
}
// A Shanghai business-day label is a calendar date, not a browser instant.
// The pagination contract uses the next calendar date as exclusive end.
export function reconciliationDayRange(value) {
  const from = requireReconciliationDate(value);
  const next = new Date(`${from}T00:00:00Z`);
  next.setUTCDate(next.getUTCDate() + 1);
  const to = requireReconciliationDate(next.toISOString().slice(0, 10));
  return { business_date_from: from, business_date_to: to };
}
export function requireReconciliationRun(item, facts) {
  requireFinanceID(item?.reconciliation_run_id);
  if (item.channel !== facts.channel || item.business_date !== facts.business_date || item.status !== 'COMPLETED') throw new Error('INVALID_FINANCE_ACK');
  return item;
}
export const RECONCILIATION_BILL_MAX_BYTES = (64 << 20) - 1024;
const BILL_FIELDS = Object.entries({ merchant_order_no: 256, provider_reference_id: 256, channel_transaction_id: 256, status: 64, currency: 16 });
function billIssue(kind, row, field) {
  return Object.assign(new Error('INVALID_RECONCILIATION_INPUT'), { kind, row, field });
}
export function reconciliationImportItems(text) {
  const encoder = new TextEncoder();
  if (typeof text !== 'string') throw billIssue('billInvalid');
  if (text.length > RECONCILIATION_BILL_MAX_BYTES || encoder.encode(text).length > RECONCILIATION_BILL_MAX_BYTES) throw billIssue('billTooLarge');
  let items;
  try { items = parseFinanceJSON(text); }
  catch (reason) { throw billIssue(reason?.message === 'INVALID_FINANCE_AMOUNT' ? 'billInvalidAmount' : 'billInvalid'); }
  if (!Array.isArray(items)) throw billIssue('billNotArray');
  if (items.length === 0) throw billIssue('billEmpty');
  if (items.length > 100000) throw billIssue('billTooMany');
  for (let index = 0; index < items.length; index++) {
    const item = items[index];
    if (!item || typeof item !== 'object' || Array.isArray(item)) throw billIssue('billInvalidRow', index + 1);
    for (const [key, limit] of BILL_FIELDS) {
      if (item[key] !== undefined && (typeof item[key] !== 'string' || encoder.encode(item[key]).length > limit)) throw billIssue('billInvalidField', index + 1, key);
    }
    // Go's optional string fields accept null as the empty/default value. Reject
    // other JSON types before advertising a format-ready bill, without copying
    // the server's entry-type or channel business rules into this preview.
    for (const key of ['entry_type', 'channel']) {
      if (item[key] !== undefined && item[key] !== null && typeof item[key] !== 'string') throw billIssue('billInvalidField', index + 1, key);
    }
  }
  if (encoder.encode(encodeFinanceJSON(items)).length > RECONCILIATION_BILL_MAX_BYTES) throw billIssue('billTooLarge');
  return items;
}
export function reconciliationImportPreview(text) {
  const items = reconciliationImportItems(text);
  const totals = new Map();
  for (let index = 0; index < items.length; index++) {
    const row = index + 1, item = items[index];
    for (const field of ['merchant_order_no', 'currency', 'status', 'paid_at']) {
      if (typeof item[field] !== 'string' || !item[field].trim()) throw billIssue('billInvalidField', row, field);
    }
    // time.Time's JSON contract is RFC3339, not the browser's permissive date
    // parser. Validate the calendar too, without changing the original value.
    const timestamp = /^(\d{4}-\d{2}-\d{2})T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:[.,]\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$/.exec(item.paid_at);
    try {
      if (!timestamp) throw new Error('INVALID_RECONCILIATION_INPUT');
      requireReconciliationDate(timestamp[1]);
    } catch { throw billIssue('billInvalidField', row, 'paid_at'); }
    if (![item.provider_reference_id, item.channel_transaction_id].some(value => typeof value === 'string' && value.trim())) throw billIssue('billMissingReference', row);
    let amount;
    try { amount = integerMoney(item.amount_fen); }
    catch { throw billIssue('billInvalidField', row, 'amount_fen'); }
    if (amount <= 0n) throw billIssue('billInvalidField', row, 'amount_fen');
    totals.set(item.currency, (totals.get(item.currency) ?? 0n) + amount);
  }
  return { items, totals: Array.from(totals, ([currency, amount]) => ({ currency, fen: amount.toString() })) };
}
export function reconciliationCommand(action, fields, observation) {
  if (!RECONCILIATION_ACTIONS.includes(action) || [...fields.reason.trim()].length < 10) throw new Error('INVALID_RECONCILIATION_INPUT');
  const body = { action, reason: fields.reason.trim() };
  if (action === 'ASSIGN') body.owner_id = requireFinanceID(fields.owner.trim());
  if (action === 'SET_REPAIR_ACTION') { if (!fields.repair.trim()) throw new Error('INVALID_RECONCILIATION_INPUT'); body.repair_action = fields.repair.trim(); }
  if (action === 'RESOLVE' || action === 'IGNORE') { if (!fields.review.trim()) throw new Error('INVALID_RECONCILIATION_INPUT'); body.review_result = fields.review.trim(); }
  if (action === 'IGNORE' && observation) body.observed_observation_id = requireFinanceID(observation.observation_id);
  return body;
}

// Only explicit input-validation responses describe editable request fields.
// A cursor or state conflict still requires rereading the existing facts.
export function reconciliationReadError(error) {
  if (error.status === 403) return 'permissions';
  if (error.status === 422 && error.code === 'PAGINATION_FILTER_REQUIRED') return 'narrowFilters';
  if (error.status === 400 && ['INVALID_REQUEST', 'INVALID_FILTER'].includes(error.code)) return 'invalid';
  if (error.status === 400 || error.status === 409) return 'sourceChanged';
  return 'unavailable';
}
export function reconciliationWriteError(error) {
  if (error.message === 'INVALID_RECONCILIATION_INPUT') return 'invalid';
  if (error.code === 'COMMAND_RESULT_UNKNOWN' || !error.status || error.status >= 500) return 'runUnknown';
  if (error.status === 403) return 'permission';
  if (error.status === 400 && ['INVALID_REQUEST', 'INVALID_FILTER'].includes(error.code)) return 'invalid';
  return 'sourceChanged';
}
