export function adjustmentApprovalAction(adjustment, approval, { canRequest = false, canApprove = false } = {}) {
  const recoverable = Boolean(adjustment?.request_idempotency_key) && adjustment?.status === 'PENDING_APPROVAL';
  if (approval?.status === 'DRAFT' && canRequest && recoverable) return 'resume';
  if ((approval || adjustment?.approval_request_id) && (canRequest || canApprove)) return 'view';
  if (!approval && canRequest && recoverable) return 'resume';
  return 'none';
}

export function buildPaymentControlApprovalDraft(control, product, reason, idempotencyKey) {
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
  };
}

export function paymentControlApprovalAction(approval, { canRequest = false } = {}) {
  if (!approval) return canRequest ? 'create' : 'none';
  if (approval.status === 'DRAFT' && canRequest && (approval.allowed_actions || []).includes('SUBMIT')) return 'submit';
  if (['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING'].includes(approval.status)) return 'view';
  if (['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED', 'EXECUTION_FAILED', 'EXECUTED'].includes(approval.status)) return canRequest ? 'create' : 'view';
  return 'view';
}

export async function submitPaymentControlApproval(client, draftInput, existingApproval = null) {
  let approval = existingApproval;
  if (!approval) {
    try {
      approval = await client.createApprovalDraft(draftInput);
    } catch (createError) {
      try {
        approval = await client.createApprovalDraft(draftInput);
      } catch {
        throw createError;
      }
    }
  }
  if (approval.status === 'DRAFT') {
    if (!(approval.allowed_actions || []).includes('SUBMIT')) approval = await client.approvalRequest(approval.approval_request_id);
    if (!(approval.allowed_actions || []).includes('SUBMIT')) {
      const error = new Error('APPROVAL_SUBMIT_NOT_ALLOWED');
      error.code = 'APPROVAL_SUBMIT_NOT_ALLOWED';
      throw error;
    }
    try {
      return await client.submitApprovalRequest(approval.approval_request_id, approval.version);
    } catch (submitError) {
      const refreshed = await client.approvalRequest(approval.approval_request_id);
      if (refreshed && ['SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED'].includes(refreshed.status)) return refreshed;
      throw submitError;
    }
  }
  if (['SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED'].includes(approval.status)) return approval;
  const error = new Error(`APPROVAL_${approval.status || 'UNKNOWN'}`);
  error.code = 'APPROVAL_NOT_ACTIVE';
  throw error;
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
