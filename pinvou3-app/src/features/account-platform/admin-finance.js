import { createAndSubmitGovernedApproval, submitExistingGovernedApproval, supersedesEligibleApproval, updateAndSubmitGovernedApproval } from './admin-governed-approval-workflow.js';

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

export async function editWalletAdjustmentApproval(client, item, approval, reason) {
  const trimmed = String(reason || '').trim();
  if (!approval || approval.status !== 'DRAFT' || trimmed.length < 10 || trimmed.length > 2000) {
    throw Object.assign(new Error('adjustment approval is not editable'), { code: 'APPROVAL_DRAFT_NOT_EDITABLE' });
  }
  const page = await client.adjustments({ wallet_id: item.wallet_id, status: 'PENDING_APPROVAL', limit: 200 });
  const fresh = (page?.items || []).find((value) => value.adjustment_id === item.adjustment_id);
  if (!fresh || fresh.status !== 'PENDING_APPROVAL' || fresh.wallet_id !== item.wallet_id ||
    !Number.isSafeInteger(fresh.version) || fresh.version < 1 || !fresh.request_idempotency_key) {
    throw Object.assign(new Error('adjustment facts are no longer editable'), { code: 'ADJUSTMENT_FACTS_CHANGED' });
  }
  return updateAndSubmitGovernedApproval(client, approval,
    walletAdjustmentApprovalDraft(fresh, trimmed, fresh.request_idempotency_key));
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

export function paymentControlApprovalAction(approval, { canRequest = false } = {}) {
  if (!approval) return canRequest ? 'create' : 'none';
  // List summaries intentionally omit actions. The write path re-reads the detail before submission.
  if (approval.status === 'DRAFT' && canRequest) return 'submit';
  if (['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING'].includes(approval.status)) return 'view';
  if (approval.status === 'EXECUTION_FAILED') return 'view';
  if (['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED', 'EXECUTED'].includes(approval.status)) return canRequest ? 'create' : 'view';
  return 'view';
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

export function paymentControlSupersedesRequest(page, commandType, targetID) {
  return supersedesEligibleApproval(page, commandType, targetID);
}

export async function submitPaymentControlApproval(client, draftInput, existingApproval = null) {
  if (existingApproval) return submitExistingGovernedApproval(client, existingApproval);
  if (!draftInput) throw Object.assign(new Error('approval draft is unavailable'), { code: 'INVALID_APPROVAL_REQUEST' });
  return createAndSubmitGovernedApproval(client, draftInput);
}

export function updateFinanceTabState(current, surface, tab, patch) {
  return {
    ...(current || {}),
    financeTabStates: {
      ...(current?.financeTabStates || {}),
      [surface]: {
        ...(current?.financeTabStates?.[surface] || {}),
        [tab]: {
          ...(current?.financeTabStates?.[surface]?.[tab] || {}),
          ...patch,
        },
      },
    },
  };
}

export function readFinanceTabQuery(current, surface, tab, fallback = {}) {
  const cached = current?.financeTabStates?.[surface]?.[tab]?.query;
  return { ...(cached || fallback) };
}

export function financeTabShouldLoad(state = {}, desiredQuery = null) {
  if (state.loading) return false;
  if (!state.attempted) return true;
  if (!desiredQuery) return false;
  const requestedQuery = state.requestedQuery || state.query || null;
  return JSON.stringify(requestedQuery) !== JSON.stringify(desiredQuery);
}

export function resetFinanceSurfaceLoading(current, surface) {
  const surfaceStates = current?.financeTabStates?.[surface] || {};
  const nextSurfaceStates = Object.fromEntries(Object.entries(surfaceStates).map(([tab, state]) => [
    tab,
    state.loading ? { ...state, loading: false, attempted: Boolean(state.loaded) } : state,
  ]));
  const next = {
    ...(current || {}),
    financeTabStates: {
      ...(current?.financeTabStates || {}),
      [surface]: nextSurfaceStates,
    },
  };
  if (surface === 'payments' && next.paymentCapabilityState?.loading) {
    next.paymentCapabilityState = { ...next.paymentCapabilityState, loading: false, attempted: false, loaded: false };
    next.paymentCapabilities = null;
  }
  return next;
}

export function updatePaymentCapabilityState(current, statePatch, capabilities = current?.paymentCapabilities ?? null) {
  return {
    ...(current || {}),
    paymentCapabilities: capabilities,
    paymentCapabilityState: {
      ...(current?.paymentCapabilityState || {}),
      ...statePatch,
    },
  };
}

export function createFinanceTabRequestTracker() {
  const generations = new Map();
  return {
    begin(key) {
      const generation = (generations.get(key) || 0) + 1;
      generations.set(key, generation);
      return generation;
    },
    isCurrent(key, generation) {
      return generations.get(key) === generation;
    },
    invalidate(key) {
      generations.set(key, (generations.get(key) || 0) + 1);
    },
    invalidatePrefix(prefix) {
      for (const key of generations.keys()) if (key.startsWith(prefix)) this.invalidate(key);
    },
    invalidateAll() {
      for (const key of generations.keys()) this.invalidate(key);
    },
  };
}
