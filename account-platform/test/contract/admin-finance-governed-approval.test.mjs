import assert from 'node:assert/strict';
import test from 'node:test';
import { buildPaymentControlApprovalDraft, paymentControlApprovalAction, paymentControlSupersedesRequest } from '../../../pinvou3-app/src/features/account-platform/admin-finance.js';

const id = '0199a300-0001-7000-8000-000000000017';
const page = (status) => ({ items: [{ command_type: 'PAYMENT_PROVIDER_RESUME', target: { resource_id: 'provider' },
  approval_request_id: id, status }], has_more: false });

test('payment control replacement is allowed only after an eligible terminal request', () => {
  for (const status of ['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED']) {
    assert.equal(paymentControlSupersedesRequest(page(status), 'PAYMENT_PROVIDER_RESUME', 'provider'), id);
  }
  assert.equal(paymentControlSupersedesRequest(page('EXECUTED'), 'PAYMENT_PROVIDER_RESUME', 'provider'), '');
  for (const status of ['DRAFT', 'SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTION_FAILED', 'UNKNOWN']) {
    assert.throws(() => paymentControlSupersedesRequest(page(status), 'PAYMENT_PROVIDER_RESUME', 'provider'));
  }
  assert.throws(() => paymentControlSupersedesRequest({ ...page('REJECTED'), has_more: true }, 'PAYMENT_PROVIDER_RESUME', 'provider'));
  assert.equal(paymentControlApprovalAction({ status: 'EXECUTION_FAILED' }, { canRequest: true }), 'view');
});

test('fresh payment control draft links the old request without inheriting its frozen facts', () => {
  const control = { provider_id: 'provider', provider_control_version: 3,
    resume_draft: { command_type: 'PAYMENT_PROVIDER_RESUME', target_resource_type: 'PAYMENT_PROVIDER_CONTROL',
      target_resource_id: 'provider', normalized_command: { provider_control_version: 3 } } };
  const draft = buildPaymentControlApprovalDraft(control, null, 'fresh verified reason', 'new-key', id);
  assert.equal(draft.supersedes_request_id, id);
  assert.equal(draft.idempotency_key, 'new-key');
  assert.equal(draft.normalized_command.provider_control_version, 3);
  assert.equal(draft.financial_snapshot.provider_control_version, 3);
});
