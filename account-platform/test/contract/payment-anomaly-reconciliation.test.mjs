import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('payment anomalies preserve evidence and use idempotent reviewed commands', async () => {
  const [migration, anomalies, chargebacks, refunds, service, openapi] = await Promise.all([
    read('migrations/billing/000029_payment_anomaly_reconciliation.up.sql'),
    read('internal/billing/payment_anomalies.go'),
    read('internal/billing/chargebacks.go'),
    read('internal/billing/forced_refunds.go'),
    read('internal/billing/service.go'),
    read('contracts/openapi/admin.yaml'),
  ]);
  for (const value of ['CHARGEBACK', 'FORCED_REFUND', 'DUPLICATE_PAYMENT', 'LATE_PAYMENT_CONFLICT', 'PAYMENT_FACT_CONFLICT', 'REFUND_FAILURE', 'ILLEGAL_TRANSITION']) assert.match(migration, new RegExp(value));
  assert.match(migration, /payment_anomaly_commands/);
  assert.match(migration, /reconciliation_case_commands/);
  assert.match(migration, /prevent_payment_review_evidence_mutation/);
  assert.match(anomalies, /billing\.payment-anomaly\.review-required\.v1/);
  assert.match(anomalies, /lockTransactionKey/);
  assert.match(chargebacks, /status='REVIEW_REQUIRED'/);
  assert.match(refunds, /ApplyReviewedForcedRefund/);
  for (const path of ['payment-anomalies/:case_id/claim', 'payment-anomalies/:case_id/resolve', 'reconciliation-cases/:case_id/commands']) assert.match(service, new RegExp(path));
  assert.match(openapi, /immutable evidence, owner, review conclusion, and financial-action status/);
});

test('daily four-way reconciliation keeps independent runs and compares ledger facts', async () => {
  const [migration, reconciliation, provider, adminUI, financialUI, reconciliationUI, i18n] = await Promise.all([
    read('migrations/billing/000029_payment_anomaly_reconciliation.up.sql'),
    read('internal/billing/reconciliation.go'),
    read('internal/platform/payment/provider.go'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/AdminFinancialOperations.jsx'),
    read('../pinvou3-app/src/features/account-platform/AdminReconciliation.jsx'),
    read('../pinvou3-app/src/shared/admin-platform-i18n.js'),
  ]);
  assert.match(migration, /reconciliation_runs_scheduled_once_idx/);
  assert.match(migration, /CREATE TABLE reconciliation_provider_rows/);
  assert.match(migration, /reconciliation_provider_rows_immutable/);
  assert.match(reconciliation, /CompareFourWayReconciliation/);
  assert.match(reconciliation, /DailyPaymentReconciliationWorker/);
  assert.match(reconciliation, /reconciliationBusinessDateRange/);
  assert.match(reconciliation, /payment-reconciliation/);
  assert.match(reconciliation, /RunAdminReconciliation[\s\S]*MANUAL_API[\s\S]*&actorID/);
  assert.match(reconciliation, /INSERT INTO reconciliation_provider_rows/);
  assert.doesNotMatch(reconciliation, /DELETE FROM reconciliation_cases/);
  assert.match(provider, /ProviderReferenceID/);
  assert.match(adminUI, /<AdminFinancialOperations/);
  assert.match(adminUI, /<AdminReconciliation/);
  assert.match(financialUI, /client\.paymentAnomalies/);
  assert.match(reconciliationUI, /client\.updateReconciliationCase/);
  assert.match(reconciliationUI, /action === 'ASSIGN'/);
  for (const snapshot of ['provider_snapshot', 'internal_snapshot', 'refund_snapshot', 'ledger_snapshot']) assert.match(reconciliationUI, new RegExp(snapshot));
  for (const locale of ['zh', 'en', 'ja']) assert.match(i18n, new RegExp(`adminPlatformCopy\\.${locale}\\.billing, \\{ anomalies:`));
});
