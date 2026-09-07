import { secureRequestID } from './request-id.js';
export const PAYMENT_ANOMALY_RESOLUTIONS = Object.freeze({
  CHARGEBACK: Object.freeze(['CONFIRM_CHARGEBACK', 'DISMISS', 'ESCALATE_FINANCIAL_REPAIR']),
  FORCED_REFUND: Object.freeze(['CONFIRM_FORCED_REFUND', 'DISMISS', 'ESCALATE_FINANCIAL_REPAIR']),
  DUPLICATE_PAYMENT: Object.freeze(['CONFIRM_PAYMENT', 'ESCALATE_FINANCIAL_REPAIR']),
  LATE_PAYMENT_CONFLICT: Object.freeze(['CONFIRM_PAYMENT', 'ESCALATE_FINANCIAL_REPAIR']),
  PAYMENT_FACT_CONFLICT: Object.freeze(['DISMISS', 'ESCALATE_FINANCIAL_REPAIR']),
  REFUND_FAILURE: Object.freeze(['DISMISS', 'ESCALATE_FINANCIAL_REPAIR']),
  ILLEGAL_TRANSITION: Object.freeze(['CONFIRM_PROVIDER_FAILED', 'ESCALATE_FINANCIAL_REPAIR']),
});

export function paymentAnomalyResolutionOptions(item = {}) {
  if (item.status !== 'OPEN') return [];
  return [...(PAYMENT_ANOMALY_RESOLUTIONS[item.anomaly_type] || [])];
}

export function financeActionRequiresRefresh(operation = {}) {
  return operation.writeSucceeded === true || operation.outcomeUnknown === true;
}

export function createFinanceIdempotencyKey(prefix = 'finance-action') {
  return `${prefix}-${secureRequestID()}-${secureRequestID()}`;
}

export function isUnknownMutationOutcome(error) {
  return !Number.isInteger(error?.status) || error.status >= 500;
}

export async function executeStructuredFinanceAction({ execute, refresh }) {
  const result = await execute();
  try {
    await refresh();
    return { result, refreshed: true, refreshError: null };
  } catch (refreshError) {
    return { result, refreshed: false, refreshError };
  }
}
