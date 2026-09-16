import { createAndSubmitGovernedApproval, submitExistingGovernedApproval } from './admin-governed-approval-workflow.js';

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
  if (!page || !Array.isArray(page.items) || page.has_more === true || page.items.length >= 200) {
    throw Object.assign(new Error('approval history is incomplete'), { code: 'APPROVAL_HISTORY_INCOMPLETE' });
  }
  const matching = page.items.filter((item) => item.command_type === commandType &&
    (item.target?.resource_id || item.target_resource_id) === targetID);
  const current = matching[0]; // The Account list is ordered by updated_at DESC, id DESC.
  if (!current) return '';
  if (['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTION_FAILED'].includes(current.status)) {
    throw Object.assign(new Error('an approval is already open or awaiting recovery'), { code: 'APPROVAL_ALREADY_ACTIVE' });
  }
  if (current.status === 'EXECUTED') return '';
  if (['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED'].includes(current.status) && current.approval_request_id) {
    return current.approval_request_id;
  }
  throw Object.assign(new Error('approval state is not eligible for replacement'), { code: 'APPROVAL_REPLACEMENT_UNAVAILABLE' });
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
