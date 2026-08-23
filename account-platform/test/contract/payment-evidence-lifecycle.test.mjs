import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('payment orders own many immutable provider transactions and evidence records', async () => {
  const migration = await read('migrations/billing/000024_payment_evidence_lifecycle.up.sql');
  assert.match(migration, /DROP INDEX payment_transactions_order_idx/);
  assert.match(migration, /CREATE INDEX payment_transactions_order_idx\s+ON payment_transactions/);
  assert.doesNotMatch(migration, /CREATE UNIQUE INDEX payment_transactions_order_idx/);
  assert.match(migration, /CREATE TABLE payment_evidence/);
  assert.match(migration, /payment_evidence_immutable/);
  assert.match(migration, /source_payment_transaction_id UUID REFERENCES payment_transactions/);
  assert.match(migration, /entitlement_packages_payment_transaction_idx/);
});

test('callbacks queries and repairs share one transaction convergence path', async () => {
  const payments = await read('internal/billing/payments.go');
  const reconciliation = await read('internal/billing/reconciliation.go');
  const merger = await read('internal/billing/payment_evidence.go');
  assert.match(payments, /mergeSuccessfulPaymentWithinTransaction\(ctx, tx, order, "CALLBACK"/);
  assert.match(reconciliation, /mergeSuccessfulPaymentWithinTransaction\(ctx, tx, order, "QUERY"/);
  assert.match(reconciliation, /mergeSuccessfulPaymentWithinTransaction\(ctx, tx, order, "MANUAL_REPAIR"/);
  assert.match(merger, /priorTransactions > 0/);
  assert.match(merger, /issuePackageWithinTransaction\(ctx, tx, order, transaction\.ID/);
  assert.match(merger, /"payment_transaction_id": transaction\.ID/);
});

test('payment refund and close lifecycles do not overwrite provider payment facts', async () => {
  const refunds = await read('internal/billing/refunds.go');
  const migration = await read('migrations/billing/000024_payment_evidence_lifecycle.up.sql');
  assert.doesNotMatch(refunds, /UPDATE payment_transactions SET status=.*REFUND/);
  assert.match(refunds, /projectPaymentOrderWithinTransaction/);
  assert.match(migration, /close_status TEXT NOT NULL DEFAULT 'OPEN'/);
  assert.match(migration, /payment_summary JSONB NOT NULL/);
  assert.match(migration, /refund_summary JSONB NOT NULL/);
  assert.match(migration, /chargeback_summary JSONB NOT NULL/);
});

test('organization payment freezes the actual payer and beneficiary independently', async () => {
  const payments = await read('internal/billing/payments.go');
  const migration = await read('migrations/billing/000024_payment_evidence_lifecycle.up.sql');
  assert.match(migration, /payer_user_id UUID/);
  assert.match(migration, /payer_commerce_mapping_snapshot JSONB/);
  assert.match(payments, /"payer_user_id": requesterID/);
  assert.match(payments, /beneficiaryOrganizationID/);
  assert.match(payments, /wallet\.OwnerType != "ORGANIZATION"/);
});

test('operations API exposes transactions immutable evidence and derived summaries', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const detail = contract.paths['/billing/admin/v1/payment-orders/{order_id}'].get;
  const schema = contract.components.schemas.PaymentOrderDetail;
  assert.ok(detail);
  assert.ok(['transactions', 'evidence', 'payment_summary', 'refund_summary', 'chargeback_summary', 'attention_summary'].every(field => schema.properties[field]));
  assert.ok(contract.components.schemas.PaymentTransaction.properties.provider_transaction_id);
  assert.ok(contract.components.schemas.PaymentEvidence.properties.payload_digest);
});

test('public contracts contain no Mall-specific order field', async () => {
  for (const file of ['contracts/openapi/billing.yaml', 'contracts/openapi/token-gateway.yaml']) {
    const source = await read(file);
    assert.doesNotMatch(source, /mall_order_id/);
    assert.match(source, /provider_order_reference/);
  }
});
