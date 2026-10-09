export const approvalCommands = Object.freeze(['WALLET_ADJUSTMENT', 'PAYMENT_PROVIDER_RESUME', 'PAYMENT_PRODUCT_RESUME', 'LEDGER_REPAIR', 'ENTITLEMENT_COMPONENT_ADJUSTMENT', 'RECEIVABLE_WRITE_OFF', 'PAYMENT_FINANCIAL_REPAIR', 'REFUND_EXCEPTION_REPAIR']);
export const approvalViews = Object.freeze(['mine', 'pending', 'execution', 'directory']);
export const executionStatuses = Object.freeze(['EXECUTION_FAILED', 'EXECUTING', 'APPROVED', 'EXECUTED']);
export const approvalStatuses = Object.freeze(['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTION_FAILED', 'EXECUTED', 'REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED']);

export function approvalCenterVisible(permissions) {
  return ['platform.billing.wallets.read', 'platform.billing.repairs.request', 'platform.billing.repairs.approve',
    'platform.billing.adjustments.approve', 'platform.billing.payment-controls.resume.request',
    'platform.billing.payment-controls.resume.approve'].some(permission => permissions.includes(permission));
}

export function readApprovalRoute(search = '') {
  const query = new URLSearchParams(search);
  const view = query.get('approval_view');
  const command = query.get('approval_command_type');
  const status = query.get('approval_status');
  return { view: approvalViews.includes(view) ? view : 'mine',
    commandType: approvalCommands.includes(command) ? command : '', target: query.get('approval_target') || '',
    status: approvalStatuses.includes(status) ? status : '', requestID: query.get('approval_request_id') || '' };
}

export function approvalQuery(route, userID) {
  if (!userID) throw new Error('INVALID_APPROVAL_ACTOR');
  const query = {};
  if (route.commandType) query.command_type = route.commandType;
  if (route.target) query.target = route.target;
  if (route.view === 'mine') { query.requester_user_id = userID; if (route.status) query.status = route.status; }
  if (route.view === 'pending') query.status = 'SUBMITTED';
  if (route.view === 'execution') query.status = executionStatuses.includes(route.status) ? route.status : 'EXECUTION_FAILED';
  return query;
}

export function approvalRequester(item) { return item?.requester?.user_id || item?.requester_user_id || ''; }

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
export function approvalBusinessRoute(detail) {
  if (detail?.detail_availability !== 'AVAILABLE' || !detail.command || !detail.target) return null;
  const target = detail.target;
  switch (detail.command_type) {
    case 'ENTITLEMENT_COMPONENT_ADJUSTMENT':
      return target.resource_type === 'ENTITLEMENT_PACKAGE' && uuid.test(target.resource_id) && detail.command.package_id === target.resource_id
        ? { section: 'finance', tab: 'correction' } : null;
    case 'REFUND_EXCEPTION_REPAIR':
      return target.resource_type === 'REFUND' && uuid.test(target.resource_id) && detail.command.refund_id === target.resource_id
        ? { section: 'finance', tab: 'refundrepair' } : null;
    case 'PAYMENT_FINANCIAL_REPAIR':
      return target.resource_type === 'PAYMENT_ORDER' && uuid.test(target.resource_id) && detail.command.payment_order_id === target.resource_id
        ? { section: 'finance', tab: 'paymentrepair' } : null;
    case 'RECEIVABLE_WRITE_OFF':
      return target.resource_type === 'RECEIVABLE' && uuid.test(target.resource_id) && detail.command.receivable_id === target.resource_id
        ? { section: 'finance', tab: 'writeoff' } : null;
    case 'LEDGER_REPAIR':
      return target.resource_type === 'LEDGER_POSTING' && uuid.test(target.resource_id) && detail.command.target_posting_id === target.resource_id
        ? { section: 'finance', tab: 'ledger' } : null;
    case 'WALLET_ADJUSTMENT':
      return target.resource_type === 'MANUAL_ADJUSTMENT' && uuid.test(target.resource_id) && detail.command.adjustment_id === target.resource_id
        ? { section: 'finance', tab: 'wallets' } : null;
    case 'PAYMENT_PROVIDER_RESUME':
      return target.resource_type === 'PAYMENT_PROVIDER_CONTROL' && !!target.resource_id && detail.command.provider_id === target.resource_id
        ? { section: 'finance', tab: 'controls' } : null;
    case 'PAYMENT_PRODUCT_RESUME':
      return target.resource_type === 'PAYMENT_PRODUCT_CONTROL' && !!target.resource_id && detail.command.product_id === target.resource_id
        ? { section: 'finance', tab: 'controls' } : null;
    default: return null;
  }
}
