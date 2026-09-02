import assert from 'node:assert/strict';
import test from 'node:test';

import {
  createFinanceIdempotencyKey,
  executeStructuredFinanceAction,
  financeActionRequiresRefresh,
  isUnknownMutationOutcome,
  paymentAnomalyResolutionOptions,
} from '../src/features/account-platform/finance-actions.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

test('payment anomaly actions match every server anomaly transition', () => {
  const expected = {
    CHARGEBACK: ['CONFIRM_CHARGEBACK', 'DISMISS', 'ESCALATE_FINANCIAL_REPAIR'],
    FORCED_REFUND: ['CONFIRM_FORCED_REFUND', 'DISMISS', 'ESCALATE_FINANCIAL_REPAIR'],
    DUPLICATE_PAYMENT: ['CONFIRM_PAYMENT', 'ESCALATE_FINANCIAL_REPAIR'],
    LATE_PAYMENT_CONFLICT: ['CONFIRM_PAYMENT', 'ESCALATE_FINANCIAL_REPAIR'],
    PAYMENT_FACT_CONFLICT: ['DISMISS', 'ESCALATE_FINANCIAL_REPAIR'],
    REFUND_FAILURE: ['DISMISS', 'ESCALATE_FINANCIAL_REPAIR'],
    ILLEGAL_TRANSITION: ['CONFIRM_PROVIDER_FAILED', 'ESCALATE_FINANCIAL_REPAIR'],
  };
  for (const [anomalyType, resolutions] of Object.entries(expected)) {
    assert.deepEqual(paymentAnomalyResolutionOptions({ anomaly_type: anomalyType, status: 'OPEN', financial_action_status: 'NONE' }), resolutions);
  }
  assert.deepEqual(paymentAnomalyResolutionOptions({ anomaly_type: 'CHARGEBACK', status: 'RESOLVED' }), []);
  assert.deepEqual(paymentAnomalyResolutionOptions({ anomaly_type: 'CHARGEBACK' }), []);
  assert.deepEqual(paymentAnomalyResolutionOptions({ anomaly_type: 'CHARGEBACK', status: '' }), []);
  assert.deepEqual(paymentAnomalyResolutionOptions({ anomaly_type: 'CHARGEBACK', status: 'UNKNOWN' }), []);
  assert.deepEqual(paymentAnomalyResolutionOptions({ anomaly_type: 'UNKNOWN', status: 'OPEN' }), []);
});

test('a successful write is never replayed when its refresh fails', async () => {
  let writes = 0;
  let refreshes = 0;
  const refreshError = Object.assign(new Error('refresh failed'), { status: 503 });
  const outcome = await executeStructuredFinanceAction({
    execute: async () => { writes += 1; return { version: 2 }; },
    refresh: async () => { refreshes += 1; throw refreshError; },
  });
  assert.equal(writes, 1);
  assert.equal(refreshes, 1);
  assert.equal(outcome.refreshed, false);
  assert.equal(outcome.refreshError, refreshError);
});

test('a failed write never starts a refresh', async () => {
  let refreshes = 0;
  await assert.rejects(executeStructuredFinanceAction({
    execute: async () => { throw Object.assign(new Error('conflict'), { status: 409 }); },
    refresh: async () => { refreshes += 1; },
  }), /conflict/);
  assert.equal(refreshes, 0);
});

test('unknown outcomes and generated idempotency keys are explicit', () => {
  assert.equal(isUnknownMutationOutcome(new TypeError('network failed')), true);
  assert.equal(isUnknownMutationOutcome({ status: 503 }), true);
  assert.equal(isUnknownMutationOutcome({ status: 409 }), false);
  const first = createFinanceIdempotencyKey('refund');
  const second = createFinanceIdempotencyKey('refund');
  assert.match(first, /^refund-/);
  assert.notEqual(first, second);
});

test('unknown and written-but-unrefreshed actions cannot be closed and reopened', () => {
  assert.equal(financeActionRequiresRefresh({}), false);
  assert.equal(financeActionRequiresRefresh({ outcomeUnknown: true }), true);
  assert.equal(financeActionRequiresRefresh({ writeSucceeded: true }), true);
  assert.equal(financeActionRequiresRefresh({ outcomeUnknown: false, writeSucceeded: false }), false);
});

test('retryable finance clients forward the caller-frozen idempotency key', async () => {
  const originalFetch = globalThis.fetch;
  const observed = [];
  globalThis.fetch = async (_url, options) => {
    observed.push(options.headers['Idempotency-Key']);
    return { ok: true, status: 200, headers: new Headers(), json: async () => ({}) };
  };
  try {
    const client = new AdminPlatformClient('https://example.test');
    client.csrfToken = 'csrf';
    const key = createFinanceIdempotencyKey('frozen');
    await client.refundPaymentOrder('order', 'transaction', 'package', 'sufficient reason', key);
    await client.releaseHold('hold', 'sufficient reason', key);
    await client.updateReconciliationCase('case', { action: 'IGNORE', reason: 'sufficient reason' }, key);
    await client.resolvePaymentAnomaly('anomaly', 'DISMISS', 'sufficient reason', key);
    assert.deepEqual(observed, [key, key, key, key]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
