const resolutionsByType = {
  CHARGEBACK: ['CONFIRM_CHARGEBACK', 'DISMISS', 'ESCALATE_FINANCIAL_REPAIR'],
  FORCED_REFUND: ['CONFIRM_FORCED_REFUND', 'DISMISS', 'ESCALATE_FINANCIAL_REPAIR'],
  DUPLICATE_PAYMENT: ['CONFIRM_PAYMENT', 'ESCALATE_FINANCIAL_REPAIR'],
  LATE_PAYMENT_CONFLICT: ['CONFIRM_PAYMENT', 'ESCALATE_FINANCIAL_REPAIR'],
  ILLEGAL_TRANSITION: ['CONFIRM_PROVIDER_FAILED', 'ESCALATE_FINANCIAL_REPAIR'],
  PAYMENT_FACT_CONFLICT: ['DISMISS', 'ESCALATE_FINANCIAL_REPAIR'],
  REFUND_FAILURE: ['RECONCILE_REFUND', 'DISMISS', 'ESCALATE_FINANCIAL_REPAIR'],
};

export function unmatchedCallbackEvidence(item) {
  const snapshot = item?.resource_snapshot;
  return item?.anomaly_type === 'PAYMENT_FACT_CONFLICT' && !item?.payment_order_id &&
    snapshot?.reason === 'PAYMENT_ORDER_NOT_FOUND' && snapshot?.notification_id ? snapshot : null;
}

export function paymentAnomalyResolutions(type, item) {
  if (unmatchedCallbackEvidence(item)) return ['DISMISS'];
  return Object.prototype.hasOwnProperty.call(resolutionsByType, type) ? [...resolutionsByType[type]] : [];
}
