import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import { financialEventTypes, validateFinancialEvent } from '../fixtures/financial-event-validator.mjs';

const id = '00000000-0000-7000-8000-000000000001';
const time = '2026-10-22T00:00:00Z';
const payloads = {
  'account.wallet.provisioning.completed.v1': [{ user_id: id }],
  'account.wallet.provisioning.requested.v1': [
    { user_id: id, policy_version: 'wallet-provisioning-v1' },
    { user_id: id, policy_version: 'wallet-provisioning-v1', registration_operation_id: id },
    { user_id: id, policy_version: 'x'.repeat(65) },
  ],
  'billing.late-payment.review-required.v1': [{ case_id: id, payment_order_id: id, severity: 'P2', next_review_at: time }],
  'billing.manual-review.sla-escalated.v1': [
    { case_id: id, stage: 'CLAIM_OVERDUE', severity: 'P1', owner_id: null },
    { case_id: id, stage: 'RESOLUTION_OVERDUE', severity: 'P0', owner_id: id },
  ],
  'billing.payment-anomaly.review-required.v1': [
    { case_id: id, anomaly_type: 'PAYMENT_FACT_CONFLICT', severity: 'P1', payment_order_id: null },
    { case_id: id, anomaly_type: 'REFUND_FAILURE', severity: 'P0', payment_order_id: id },
  ],
  'billing.provider-blind.detected.v1': [
    { case_id: id, provider_id: 'controlled', provider_version: 'v1', reason: 'controlled provider blind observation', new_orders_enabled: false },
    { case_id: id, payment_order_id: id, provider_id: 'controlled', provider_version: 'v1', reason: '渠道异常', new_orders_enabled: false },
  ],
  'billing.recharge.succeeded.v1': [
    { order_id: id, payment_transaction_id: id, entitlement_package_id: id, payer_user_id: id, wallet_id: id, beneficiary_organization_id: null, channel: 'ALIPAY', amount_fen: 100, credit_microcredits: 1000000 },
    { order_id: id, payment_transaction_id: id, entitlement_package_id: id, payer_user_id: id, wallet_id: id, beneficiary_organization_id: id, channel: 'WECHAT', amount_fen: 100, credit_microcredits: 1000000 },
  ],
};
function event(name, payload) {
  return { event_id: id, event_type: name, aggregate_id: id, aggregate_version: name === 'account.wallet.provisioning.completed.v1' ? 2 : 1, occurred_at: time, trace_id: 'internal', payload };
}

// These controlled samples validate a contract carrier, not a live producer or
// downstream consumer. The PG test additionally feeds real Billing outbox rows
// through production Event.Marshal into this exact validator.
for (const name of financialEventTypes) {
  test(`financial event schema: ${name}`, async () => {
    const schema = JSON.parse(await readFile(new URL(`../../contracts/events/${name}.schema.json`, import.meta.url), 'utf8'));
    for (const payload of payloads[name]) {
      const value = event(name, payload);
      assert.equal(validateFinancialEvent(value), true);
      for (const key of schema.required) {
        const invalid = structuredClone(value);
        delete invalid[key];
        assert.equal(validateFinancialEvent(invalid), false, `missing envelope ${key}`);
      }
      for (const key of schema.properties.payload.required) {
        const invalid = structuredClone(value);
        delete invalid.payload[key];
        assert.equal(validateFinancialEvent(invalid), false, `missing payload ${key}`);
      }
      for (const key of Object.keys(payload)) {
        const invalid = structuredClone(value);
        invalid.payload[key] = { unexpected: true };
        assert.equal(validateFinancialEvent(invalid), false, `wrong payload type ${key}`);
      }
      assert.equal(validateFinancialEvent({ ...value, lease_until: time }), false);
      assert.equal(validateFinancialEvent({ ...value, payload: { ...payload, unknown: true } }), false);
      assert.equal(validateFinancialEvent({ ...value, aggregate_version: '1' }), false);
      assert.equal(validateFinancialEvent({ ...value, aggregate_version: 0 }), false);
      assert.equal(validateFinancialEvent({ ...value, aggregate_version: 1.5 }), false);
      assert.equal(validateFinancialEvent({ ...value, event_id: 'invalid-uuid' }), false);
      assert.equal(validateFinancialEvent({ ...value, trace_id: ' ' }), false);
      assert.equal(validateFinancialEvent({ ...value, occurred_at: 'not-a-time' }), false);
      assert.equal(validateFinancialEvent({ ...value, event_type: 'billing.unknown.v1' }), false);
    }
  });
}

test('seven contracts remain bound to the current emitting source and envelope', async () => {
  const producers = await Promise.all([
    'account/postgres.go', 'account/registration_saga.go', 'account/wallet_provisioning.go', 'securityadmin/security.go',
    'billing/late_payment_reviews.go', 'billing/payment_anomalies.go', 'billing/payment_evidence.go',
  ].map((path) => readFile(new URL(`../../internal/${path}`, import.meta.url), 'utf8')));
  for (const name of financialEventTypes) {
    assert.ok(producers.some((source) => source.includes(`"${name}"`)), `missing actual emitting subject ${name}`);
  }
  const envelope = await readFile(new URL('../../internal/platform/events/event.go', import.meta.url), 'utf8');
  for (const key of ['event_id', 'event_type', 'aggregate_id', 'aggregate_version', 'occurred_at', 'trace_id', 'payload']) {
    assert.ok(envelope.includes(`json:"${key}"`));
  }
  // These specific old-contract mismatches must remain accepted by the new
  // carrier. They do not loosen or modify any production serialization.
  assert.equal(validateFinancialEvent(event('account.wallet.provisioning.requested.v1', payloads['account.wallet.provisioning.requested.v1'][0])), true);
  assert.equal(validateFinancialEvent(event('billing.manual-review.sla-escalated.v1', payloads['billing.manual-review.sla-escalated.v1'][0])), true);
  assert.equal(validateFinancialEvent(event('billing.recharge.succeeded.v1', payloads['billing.recharge.succeeded.v1'][1])), true);
});
