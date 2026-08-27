import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('provider bill rows enter the common immutable evidence merger', async () => {
  const reconciliation = await read('internal/billing/reconciliation.go');
  const migration = await read('migrations/billing/000033_payment_convergence_states.up.sql');
  assert.match(reconciliation, /mergeProviderBillEvidenceWithinTransaction/);
  assert.match(reconciliation, /mergeSuccessfulPaymentWithinTransaction\(ctx, tx, order, "BILL"/);
  assert.match(reconciliation, /INSERT INTO payment_evidence/);
  assert.match(reconciliation, /payload_digest/);
  assert.match(migration, /payment_evidence ALTER COLUMN payment_order_id DROP NOT NULL/);
});

test('payment order close states progress monotonically and hide payment instructions', async () => {
  const refunds = await read('internal/billing/refunds.go');
  const reconciliation = await read('internal/billing/reconciliation.go');
  const migration = await read('migrations/billing/000033_payment_convergence_states.up.sql');
  for (const state of ['OPEN', 'CLOSE_REQUESTED', 'LOCALLY_CLOSED_EXTERNAL_PENDING', 'PROVIDER_CLOSED', 'EXPIRED']) {
    assert.match(migration, new RegExp(`'${state}'`));
  }
  assert.match(refunds, /close_status='CLOSE_REQUESTED'/);
  assert.match(refunds, /close_status='LOCALLY_CLOSED_EXTERNAL_PENDING'/);
  assert.match(refunds, /close_status='PROVIDER_CLOSED'/);
  assert.match(refunds, /payment_url=NULL,qr_data=NULL/);
  assert.match(reconciliation, /close_status='EXPIRED'/);
  assert.doesNotMatch(`${refunds}\n${reconciliation}`, /close_status=\$2/);
});

test('successful or review-required payments retire obsolete payment instructions', async () => {
  const convergence = await read('internal/billing/payment_evidence.go');
  const cleanup = await read('migrations/billing/000046_clear_terminal_payment_instructions.up.sql');
  assert.match(convergence, /payment_url=CASE WHEN facts\.paid_count>0 OR facts\.review_count>0 THEN NULL/);
  assert.match(convergence, /qr_data=CASE WHEN facts\.paid_count>0 OR facts\.review_count>0 THEN NULL/);
  assert.match(cleanup, /status NOT IN \('CREATED', 'PENDING'\)/);
  assert.match(cleanup, /close_status <> 'OPEN'/);
});

test('order projections derive independent payment refund chargeback and attention summaries', async () => {
  const source = await read('internal/billing/payment_evidence.go');
  assert.match(source, /FROM forced_refund_cases/);
  assert.match(source, /FROM chargeback_cases/);
  assert.match(source, /FROM receivables/);
  assert.match(source, /chargeback_summary=jsonb_build_object/);
  assert.match(source, /WHEN receivables\.open_count>0 THEN 'RECEIVABLE_OPEN'/);
  assert.doesNotMatch(source, /UPDATE payment_transactions SET status=.*REFUND/);
});

test('public and operations contracts expose the close state machine', async () => {
  const billing = YAML.parse(await read('contracts/openapi/billing.yaml'));
  const admin = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const expected = ['OPEN', 'CLOSE_REQUESTED', 'LOCALLY_CLOSED_EXTERNAL_PENDING', 'PROVIDER_CLOSED', 'EXPIRED'];
  assert.deepEqual(billing.components.schemas.RechargeOrder.properties.close_status.enum, expected);
  assert.deepEqual(billing.components.schemas.RechargeOrderHistoryItem.properties.close_status.enum, expected);
  assert.deepEqual(admin.components.schemas.PaymentOrderDetail.properties.close_status.enum, expected);
});
