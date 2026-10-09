import { financialRepairApprovalDraft } from './admin-financial-repair-drafts.js';
import { entitlementCorrectionPrecheck, entitlementCorrectionApprovalDraft } from './admin-entitlement-correction.js';
import { integerMoney, requireFinanceCollection } from './finance-contract.js';

const approvalIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

export function walletAdjustmentReplacementReference(requestKey) {
  const match = /^replacement:([0-9a-f-]{36}):([0-9a-f-]{36})$/i.exec(String(requestKey || ''));
  return match && approvalIDPattern.test(match[1]) && approvalIDPattern.test(match[2]) ? match[1] : '';
}

export function walletReplacementEligible(detail, oldAdjustmentID, walletID, requesterID) {
  const target = detail?.target || { resource_type: detail?.target_resource_type, resource_id: detail?.target_resource_id };
  const command = detail?.command || detail?.normalized_command;
  const requester = detail?.requester?.user_id || detail?.requester_user_id;
  return Boolean(requesterID && detail?.approval_request_id && requester === requesterID &&
    ['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED'].includes(detail.status) &&
    [undefined, 'AVAILABLE'].includes(detail.detail_availability) &&
    detail.command_type === 'WALLET_ADJUSTMENT' && target.resource_type === 'MANUAL_ADJUSTMENT' &&
    target.resource_id === oldAdjustmentID && command?.adjustment_id === oldAdjustmentID &&
    command?.wallet_id === walletID);
}

export function walletAdjustmentApprovalDraft(item, reason, requestKey) {
  const supersedesID = walletAdjustmentReplacementReference(requestKey);
  return {
    command_type: 'WALLET_ADJUSTMENT', idempotency_key: `approval:${requestKey}`,
    normalized_command: { adjustment_id: item.adjustment_id, wallet_id: item.wallet_id,
      amount_microcredits: item.amount_microcredits, requested_by: item.requested_by },
    target_resource_type: 'MANUAL_ADJUSTMENT', target_resource_id: item.adjustment_id,
    reason, redacted_evidence: [],
    expected_impact: { wallet_id: item.wallet_id, delta_microcredits: item.amount_microcredits },
    critical_resource_versions: { adjustment_version: item.version },
    financial_snapshot: { wallet_id: item.wallet_id, delta_microcredits: item.amount_microcredits },
    ...(supersedesID ? { supersedes_request_id: supersedesID } : {}),
  };
}

export function adjustmentApprovalAction(adjustment, approval, { canRequest = false, canApprove = false } = {}) {
  const recoverable = Boolean(adjustment?.request_idempotency_key) && adjustment?.status === 'PENDING_APPROVAL';
  if (approval?.status === 'DRAFT' && canRequest && recoverable) return 'resume';
  if ((approval || adjustment?.approval_request_id) && (canRequest || canApprove)) return 'view';
  if (!approval && canRequest && recoverable) return 'resume';
  return 'none';
}

export function buildPaymentControlApprovalDraft(control, product, reason, idempotencyKey, supersedesRequestID = '') {
  const draftInput = product ? product.resume_draft : control?.resume_draft;
  const normalizedReason = String(reason || '').trim();
  if (!control || !draftInput || normalizedReason.length < 10 || !idempotencyKey) return null;
  return {
    ...draftInput,
    idempotency_key: idempotencyKey,
    reason: normalizedReason,
    redacted_evidence: [],
    expected_impact: { provider_id: control.provider_id, product_id: product?.product_id || null, new_orders_enabled: true },
    financial_snapshot: { new_orders_enabled: false, provider_control_version: control.provider_control_version, product_control_version: product?.version || null },
    ...(supersedesRequestID ? { supersedes_request_id: supersedesRequestID } : {}),
  };
}

export async function currentApprovalEditDraft(client, detail, reason) {
  const unavailable = () => Object.assign(new Error('current owner proof is unavailable'), { status: 409, code: 'APPROVAL_FACTS_CHANGED' });
  if (!detail?.approval_request_id || detail.status !== 'DRAFT' || !detail.allowed_actions?.includes('EDIT') || String(reason).trim().length < 10) throw unavailable();
  const command = detail.command || detail.normalized_command;
  if (['TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_ROLLBACK', 'TOKEN_CREDENTIAL_REVOKE'].includes(detail.command_type)) {
    const target = detail.target || { resource_type: detail.target_resource_type, resource_id: detail.target_resource_id };
    if (!command || typeof command !== 'object' || Array.isArray(command) || !target.resource_type || !target.resource_id) throw unavailable();
    // Account re-observes Token authority on draft update and submit. Preserve
    // the approved intent, including planned times and plan versions; never
    // replace it with the UI impact endpoint's current-time projection.
    return { command_type: detail.command_type, normalized_command: command,
      target_resource_type: target.resource_type, target_resource_id: target.resource_id,
      idempotency_key: `approval-edit:${detail.approval_request_id}`, reason: reason.trim(),
      redacted_evidence: detail.redacted_evidence || [], expected_impact: detail.expected_impact || {},
      critical_resource_versions: detail.critical_resource_versions || {}, financial_snapshot: detail.financial_snapshot || {} };
  }
  if (detail.command_type === 'ENTITLEMENT_COMPONENT_ADJUSTMENT') {
    const target = detail.target || { resource_type: detail.target_resource_type, resource_id: detail.target_resource_id };
    if (!command || target.resource_type !== 'ENTITLEMENT_PACKAGE' || !approvalIDPattern.test(target.resource_id) ||
      command.package_id !== target.resource_id || !approvalIDPattern.test(command.payment_order_id) || !command.reason_code) throw unavailable();
    const facts = await client.entitlementCorrectionPrecheck(command.package_id, command.payment_order_id, command.reason_code);
    const projection = entitlementCorrectionPrecheck(facts);
    if (!projection || projection.packageID !== command.package_id || projection.orderID !== command.payment_order_id ||
      projection.reasonCode !== command.reason_code) throw unavailable();
    return entitlementCorrectionApprovalDraft(facts, command.reason_code, reason, `edit:${detail.approval_request_id}`);
  }
  if (detail.command_type === 'REFUND_EXCEPTION_REPAIR') {
    const target = detail.target || { resource_type: detail.target_resource_type, resource_id: detail.target_resource_id };
    if (!command || target.resource_type !== 'REFUND' || !approvalIDPattern.test(target.resource_id) ||
      command.refund_id !== target.resource_id || !approvalIDPattern.test(command.payment_evidence_id) ||
      !approvalIDPattern.test(command.anomaly_case_id) ||
      command.reason_code !== 'PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING') throw unavailable();
    const facts = await client.approvalCommandPrecheck({ command_type: detail.command_type,
      target_resource_id: command.refund_id, payment_evidence_id: command.payment_evidence_id,
      anomaly_case_id: command.anomaly_case_id, reason_code: command.reason_code });
    const draft = financialRepairApprovalDraft(facts, reason, `edit:${detail.approval_request_id}`);
    if (draft.command_type !== detail.command_type || draft.target_resource_id !== target.resource_id ||
      draft.normalized_command.refund_id !== command.refund_id ||
      draft.normalized_command.payment_evidence_id !== command.payment_evidence_id ||
      draft.normalized_command.anomaly_case_id !== command.anomaly_case_id) throw unavailable();
    return draft;
  }
  if (detail.command_type === 'PAYMENT_FINANCIAL_REPAIR') {
    const target = detail.target || { resource_type: detail.target_resource_type, resource_id: detail.target_resource_id };
    if (!command || target.resource_type !== 'PAYMENT_ORDER' || !approvalIDPattern.test(target.resource_id) ||
      command.payment_order_id !== target.resource_id || !approvalIDPattern.test(command.payment_evidence_id) ||
      command.reason_code !== 'CONFIRMED_PAYMENT_NOT_CREDITED') throw unavailable();
    const facts = await client.approvalCommandPrecheck({ command_type: detail.command_type,
      target_resource_id: command.payment_order_id, payment_evidence_id: command.payment_evidence_id,
      reason_code: command.reason_code });
    const draft = financialRepairApprovalDraft(facts, reason, `edit:${detail.approval_request_id}`);
    if (draft.command_type !== detail.command_type || draft.target_resource_id !== target.resource_id ||
      draft.normalized_command.payment_order_id !== command.payment_order_id ||
      draft.normalized_command.payment_evidence_id !== command.payment_evidence_id) throw unavailable();
    return draft;
  }
  if (detail.command_type === 'RECEIVABLE_WRITE_OFF') {
    const target = detail.target || { resource_type: detail.target_resource_type, resource_id: detail.target_resource_id };
    if (!command || target.resource_type !== 'RECEIVABLE' || !approvalIDPattern.test(target.resource_id) ||
      command.receivable_id !== target.resource_id || command.reason_code !== 'APPROVED_RECEIVABLE_WRITE_OFF') throw unavailable();
    const facts = await client.approvalCommandPrecheck({ command_type: 'RECEIVABLE_WRITE_OFF',
      target_resource_id: command.receivable_id, write_off_microcredits: command.write_off_microcredits,
      expected_version: command.expected_receivable_version, reason_code: command.reason_code });
    const draft = financialRepairApprovalDraft(facts, reason, `edit:${detail.approval_request_id}`);
    if (draft.command_type !== detail.command_type || draft.target_resource_id !== target.resource_id ||
      integerMoney(draft.normalized_command.write_off_microcredits) !== integerMoney(command.write_off_microcredits) ||
      integerMoney(draft.normalized_command.expected_receivable_version) !== integerMoney(command.expected_receivable_version)) throw unavailable();
    return draft;
  }
  if (detail.command_type === 'LEDGER_REPAIR') {
    const facts = await client.approvalCommandPrecheck({ command_type: 'LEDGER_REPAIR',
      target_resource_id: command?.target_posting_id, target_journal_entry_id: command?.target_journal_entry_id,
      wrong_ledger_account_id: command?.wrong_ledger_account_id, correct_ledger_account_id: command?.correct_ledger_account_id,
      amount_microcredits: command?.amount_microcredits, reason_code: 'ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT' });
    const draft = financialRepairApprovalDraft(facts, reason, `edit:${detail.approval_request_id}`);
    if (draft.target_resource_id !== (detail.target?.resource_id || detail.target_resource_id)) throw unavailable();
    return draft;
  }
  if (detail.command_type === 'WALLET_ADJUSTMENT') {
    const page = await client.adjustments({ wallet_id: command?.wallet_id, status: 'PENDING_APPROVAL', limit: 200 });
    if (page?.has_more || page?.next_cursor || !Array.isArray(page?.items) || page.items.length >= 200) throw unavailable();
    const item = requireFinanceCollection(page).find(item => item.adjustment_id === command?.adjustment_id);
    if (!item || item.status !== 'PENDING_APPROVAL' || item.wallet_id !== command.wallet_id || integerMoney(item.amount_microcredits) !== integerMoney(command.amount_microcredits) || !item.request_idempotency_key || !Number.isSafeInteger(item.version) || item.version < 1) throw unavailable();
    return walletAdjustmentApprovalDraft(item, reason.trim(), item.request_idempotency_key);
  }
  if (!['PAYMENT_PROVIDER_RESUME', 'PAYMENT_PRODUCT_RESUME'].includes(detail.command_type)) throw unavailable();
  const control = await client.paymentProviderControls();
  const product = detail.command_type === 'PAYMENT_PRODUCT_RESUME' ? control.products?.find(item => item.product_id === command?.product_id) : null;
  if (control.provider_id !== command?.provider_id || (detail.command_type === 'PAYMENT_PRODUCT_RESUME' && !product)) throw unavailable();
  const draft = buildPaymentControlApprovalDraft(control, product, reason, `approval-edit:${detail.approval_request_id}`);
  if (!draft || draft.command_type !== detail.command_type || draft.target_resource_id !== (detail.target?.resource_id || detail.target_resource_id)) throw unavailable();
  return draft;
}

export function latestApprovalByTarget(items = []) {
  const byTarget = new Map();
  // The Account list is ordered newest first; never let older history replace its current row.
  for (const item of items) {
    const targetID = item.target?.resource_id || item.target_resource_id;
    if (targetID && !byTarget.has(targetID)) byTarget.set(targetID, item);
  }
  return byTarget;
}

// Resume the exact committed adjustment, never create a second adjustment
// merely because its Account approval has not been acknowledged yet.
export async function currentPendingAdjustment(client, item, requesterID) {
  const page = await client.adjustments({ wallet_id: item.wallet_id, status: 'PENDING_APPROVAL', requested_by: requesterID, limit: 200 });
  if (page?.has_more || page?.next_cursor || !Array.isArray(page?.items) || page.items.length >= 200) throw new Error('ADJUSTMENT_FACTS_UNAVAILABLE');
  const fresh = requireFinanceCollection(page).find(value => value.adjustment_id === item.adjustment_id);
  if (!fresh || fresh.status !== 'PENDING_APPROVAL' || fresh.wallet_id !== item.wallet_id ||
    fresh.requested_by !== requesterID || fresh.request_idempotency_key !== item.request_idempotency_key ||
    integerMoney(fresh.amount_microcredits) !== integerMoney(item.amount_microcredits) || fresh.reason !== item.reason ||
    !Number.isSafeInteger(fresh.version) || fresh.version < 1 || !fresh.request_idempotency_key || String(fresh.reason || '').trim().length < 10) {
    throw Object.assign(new Error('adjustment facts changed'), { status: 409 });
  }
  return fresh;
}
