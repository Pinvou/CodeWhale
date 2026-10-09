import { integerMoney } from './finance-contract.js';
// The financial repair page is the only business entry that may create a repair
// approval draft. It never derives domain facts: the operator names a target,
// the server returns the closed owner projection, and the page submits exactly
// the closed draft the shared builder produces. Everything here is pure so the
// Node contracts can prove the closed key set without a browser.
import { financialRepairCommands, financialRepairPrecheck, financialRepairApprovalDraft } from './admin-financial-repair-drafts.js';

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const datePattern = /^\d{4}-\d{2}-\d{2}$/;

// financialRepairInputs is the closed precheck input per command: the target the
// projection is frozen on plus the evidence references the owner re-reads. It
// mirrors the Account precheck request schema exactly - same keys, same order.
export const financialRepairInputs = Object.freeze({
  LEDGER_REPAIR: Object.freeze([
    { key: 'target_resource_id', kind: 'uuid' },
    { key: 'target_journal_entry_id', kind: 'uuid' },
    { key: 'wrong_ledger_account_id', kind: 'uuid' },
    { key: 'correct_ledger_account_id', kind: 'uuid' },
    { key: 'amount_microcredits', kind: 'integer' },
  ]),
  RECEIVABLE_WRITE_OFF: Object.freeze([
    { key: 'target_resource_id', kind: 'uuid' },
    { key: 'write_off_microcredits', kind: 'integer' },
    { key: 'expected_version', kind: 'integer' },
  ]),
  PAYMENT_FINANCIAL_REPAIR: Object.freeze([
    { key: 'target_resource_id', kind: 'uuid' },
    { key: 'payment_evidence_id', kind: 'uuid' },
  ]),
  REFUND_EXCEPTION_REPAIR: Object.freeze([
    { key: 'target_resource_id', kind: 'uuid' },
    { key: 'payment_evidence_id', kind: 'uuid' },
    { key: 'anomaly_case_id', kind: 'uuid' },
  ]),
  CHARGEBACK_FINANCIAL_REPAIR: Object.freeze([
    { key: 'target_resource_id', kind: 'uuid' },
    { key: 'payment_evidence_id', kind: 'uuid' },
  ]),

});

export const financialRepairCommandTypes = Object.freeze(Object.keys(financialRepairInputs));

function closedValue(value, kind) {
  if (typeof value !== 'string') return false;
  const text = value.trim();
  if (!text || text !== value) return false;
  if (kind === 'uuid') return uuidPattern.test(text);
  if (kind === 'date') {
    if (!datePattern.test(text)) return false;
    const [year, month, day] = text.split('-').map(Number);
    const parsed = new Date(Date.UTC(year, month - 1, day));
    return parsed.getUTCFullYear() === year && parsed.getUTCMonth() === month - 1 && parsed.getUTCDate() === day;
  }
  try { return integerMoney(text) > 0n; } catch { return false; }
}

// financialRepairPrecheckBody builds the closed read-only precheck input. Any
// missing, padded or malformed field fails closed instead of being trimmed into
// something the owner would then have to reject.
export function financialRepairPrecheckBody(commandType, values = {}) {
  const fields = financialRepairInputs[commandType];
  if (!fields) return { error: 'FINANCIAL_REPAIR_PRECHECK_INVALID' };
  const body = { command_type: commandType };
  for (const field of fields) {
    const value = values[field.key];
    if (!closedValue(value, field.kind)) return { error: 'FINANCIAL_REPAIR_PRECHECK_INVALID', field: field.key };
    body[field.key] = value;
  }
  body.reason_code = financialRepairCommands[commandType].reasonCode;
  return { body };
}

// financialRepairProjectionRows renders only what the owner projection carries:
// the target plus the closed normalized keys in descriptor order. The critical
// and financial documents are shown verbatim, never recomputed.
export function financialRepairProjectionRows(projection) {
  const closed = financialRepairPrecheck(projection);
  if (!closed) return null;
  const rows = [
    { key: 'target', value: `${closed.targetResourceType} · ${closed.targetResourceID}` },
  ];
  for (const field of closed.command.fields) {
    rows.push({ key: field.key, value: String(closed.normalized[field.key]) });
  }
  rows.push({ key: 'reason_code', value: String(closed.normalized[closed.command.reasonKey] || closed.command.reasonCode) });
  return {
    rows,
    critical: JSON.stringify(closed.critical, null, 2),
    financial: JSON.stringify(closed.financial, null, 2),
  };
}

// financialRepairDraftFromProjection is the only draft the page may submit. It
// refuses an unusable projection or a reason/key outside the closed bounds.
export function financialRepairDraftFromProjection(projection, operatorReason, requestKey) {
  try {
    return { draft: financialRepairApprovalDraft(projection, operatorReason, requestKey) };
  } catch {
    return { error: 'FINANCIAL_REPAIR_PRECHECK_INVALID' };
  }
}
