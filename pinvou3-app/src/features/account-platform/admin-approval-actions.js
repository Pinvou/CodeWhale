export function approvalDetailForRoute(detail, route) {
  return route?.view !== 'directory' && route?.requestID && detail?.approval_request_id === route.requestID
    ? detail : null;
}

export function approvalActionsForDetail(detail, currentUserID) {
  if (!detail || !currentUserID || detail.detail_availability === 'DETAIL_PRESENTATION_UNAVAILABLE' ||
    !Array.isArray(detail.allowed_actions)) return [];
  const requester = detail.requester?.user_id || detail.requester_user_id;
  if (!requester) return [];
  return [...new Set(detail.allowed_actions)].filter((name) => {
    if (name === 'APPROVE' || name === 'REJECT') return requester !== currentUserID && detail.status === 'SUBMITTED';
    if (name === 'SUBMIT') return requester === currentUserID && detail.status === 'DRAFT' && detail.version === 1;
    if (name === 'WITHDRAW') return requester === currentUserID && detail.status === 'SUBMITTED';
    return false;
  });
}

export function approvalDecisionForAction(action) {
  if (action === 'APPROVE') return 'APPROVED';
  if (action === 'REJECT') return 'REJECTED';
  return null;
}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

export function approvalBusinessRoute(detail, { modelDetailsEnabled = false, tokenDetailsEnabled = false } = {}) {
  if (detail?.detail_availability !== 'AVAILABLE' || !detail.command || !detail.target) return null;
  const target = detail.target;
  switch (detail.command_type) {
    case 'WALLET_ADJUSTMENT':
      return target.resource_type === 'MANUAL_ADJUSTMENT' && uuid.test(target.resource_id) &&
        detail.command.adjustment_id === target.resource_id ? { section: 'billing', query: { finance_tab: 'adjustments' } } : null;
    case 'PAYMENT_PROVIDER_RESUME':
      return target.resource_type === 'PAYMENT_PROVIDER_CONTROL' && detail.command.provider_id === target.resource_id
        ? { section: 'payments', query: { finance_tab: 'controls' } } : null;
    case 'PAYMENT_PRODUCT_RESUME':
      return target.resource_type === 'PAYMENT_PRODUCT_CONTROL' && detail.command.product_id === target.resource_id
        ? { section: 'payments', query: { finance_tab: 'controls' } } : null;
    case 'TOKEN_MODEL_LIFECYCLE_CHANGE':
      return target.resource_type === 'MODEL' && uuid.test(target.resource_id) && detail.command.model_id === target.resource_id
        ? { section: 'models', query: modelDetailsEnabled ? { model_id: target.resource_id } : {} } : null;
    case 'TOKEN_MODEL_CATALOG_ROLLBACK':
      return target.resource_type === 'MODEL_CATALOG' && detail.command.source_catalog_version === target.resource_id
        ? { section: 'models', query: {} } : null;
    case 'TOKEN_CREDENTIAL_REVOKE':
      return target.resource_type === 'PROVIDER_CREDENTIAL' && uuid.test(target.resource_id) &&
        detail.command.credential_id === target.resource_id && uuid.test(detail.command.token_account_id)
        ? { section: 'tokenResources', query: tokenDetailsEnabled ? { token_account_id: detail.command.token_account_id } : {} } : null;
    default:
      return null;
  }
}
