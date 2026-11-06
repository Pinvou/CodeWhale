import { requireFinanceID, integerMoney } from './finance-contract.js';
import { requireReconciliationDate } from './reconciliation-contract.js';

// This read model identifies source facts. It never confers repair eligibility
// and is never used to construct the frozen financial proof.
export function requireReconciliationRepairContext(value, caseID) {
  requireFinanceID(caseID);
  if (!value || typeof value !== 'object' || Array.isArray(value) || value.case_id !== caseID) throw new Error('INVALID_RECONCILIATION_CONTEXT');
  requireFinanceID(value.case_id);
  requireFinanceID(value.reconciliation_run_id);
  requireReconciliationDate(value.business_date);
  for (const key of ['case_type', 'status', 'channel', 'provider_id', 'provider_version', 'merchant_order_no']) {
    if (typeof value[key] !== 'string' || value[key].trim() !== value[key] || !value[key]) throw new Error('INVALID_RECONCILIATION_CONTEXT');
  }
  if (typeof value.case_version !== 'string' || integerMoney(value.case_version) <= 0n ||
      typeof value.snapshot_available !== 'boolean' || typeof value.currency !== 'string' ||
      typeof value.provider_transaction_id !== 'string') throw new Error('INVALID_RECONCILIATION_CONTEXT');
  if (value.snapshot_available) {
    if (typeof value.amount_fen !== 'string') throw new Error('INVALID_RECONCILIATION_CONTEXT');
    integerMoney(value.amount_fen);
  } else if (value.amount_fen !== null || value.currency !== '' || value.provider_transaction_id !== '') throw new Error('INVALID_RECONCILIATION_CONTEXT');
  return value;
}

// The registered portal has one retained manual host and one selected-case host.
// Keep the legacy bucket intact and retain case intent by its immutable source
// ID, never by an editable input, locale or random mount identifier.
export function reconciliationRepairHost(sourceCaseID = '') {
  const scope = 'financial-repair:RECONCILIATION_FINANCIAL_REPAIR';
  if (sourceCaseID === '') return { scope, initialValues: {}, targetLocked: false };
  requireFinanceID(sourceCaseID);
  return { scope: `${scope}:case:${sourceCaseID}`, initialValues: { target_resource_id: sourceCaseID }, targetLocked: true };
}

// Only an accepted same-case source may populate the raw calendar label. An
// operator edit takes precedence, including clearing or re-entering that date.
export function reconciliationSourceBusinessDate(values, source, manuallyEdited, previousAutomaticDate = '') {
  if (manuallyEdited || typeof values?.target_resource_id !== 'string' || !source ||
      values.target_resource_id.toLowerCase() !== source.case_id) return null;
  try { requireReconciliationRepairContext(source, source.case_id); } catch { return null; }
  const current = values.business_date === undefined ? '' : values.business_date;
  if (typeof current !== 'string') return null;
  if (current !== '' && current !== previousAutomaticDate) return null;
  return source.business_date;
}
