import { encodeFinanceJSON, parseFinanceJSON, requireFinanceID } from './finance-contract.js';

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
export function reconciliationImportItems(text) {
  if (typeof text !== 'string' || new TextEncoder().encode(text).length > (64 << 20) - 1024) throw new Error('INVALID_RECONCILIATION_INPUT');
  const items = parseFinanceJSON(text);
  if (!Array.isArray(items) || items.length === 0 || items.length > 100000) throw new Error('INVALID_RECONCILIATION_INPUT');
  for (const item of items) {
    if (!item || typeof item !== 'object' || Array.isArray(item)) throw new Error('INVALID_RECONCILIATION_INPUT');
    for (const [key, limit] of Object.entries({ merchant_order_no: 256, provider_reference_id: 256, channel_transaction_id: 256, status: 64, currency: 16 })) {
      if (item[key] !== undefined && (typeof item[key] !== 'string' || new TextEncoder().encode(item[key]).length > limit)) throw new Error('INVALID_RECONCILIATION_INPUT');
    }
  }
  if (new TextEncoder().encode(encodeFinanceJSON(items)).length > (64 << 20) - 1024) throw new Error('INVALID_RECONCILIATION_INPUT');
  return items;
}
export function reconciliationCommand(action, fields) {
  if (!RECONCILIATION_ACTIONS.includes(action) || [...fields.reason.trim()].length < 10) throw new Error('INVALID_RECONCILIATION_INPUT');
  const body = { action, reason: fields.reason.trim() };
  if (action === 'ASSIGN') body.owner_id = requireFinanceID(fields.owner.trim());
  if (action === 'SET_REPAIR_ACTION') { if (!fields.repair.trim()) throw new Error('INVALID_RECONCILIATION_INPUT'); body.repair_action = fields.repair.trim(); }
  if (action === 'RESOLVE' || action === 'IGNORE') { if (!fields.review.trim()) throw new Error('INVALID_RECONCILIATION_INPUT'); body.review_result = fields.review.trim(); }
  return body;
}
