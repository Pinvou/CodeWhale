import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('wallets project independent receivables into signed availability and context freeze', async () => {
  const migration = await read('migrations/billing/000025_receivables_and_context_freeze.up.sql');
  const model = await read('internal/billing/model.go');
  const repository = await read('internal/billing/postgres.go');
  assert.match(migration, /receivable_microcredits BIGINT NOT NULL DEFAULT 0/);
  assert.match(migration, /financial_status IN \('CLEAR','RECEIVABLE_OPEN'\)/);
  assert.match(model, /ReceivableMicrocredits int64/);
  assert.match(repository, /posted_microcredits-reserved_microcredits-receivable_microcredits/);
  assert.match(repository, /receivable_microcredits=0/);
});

test('usage overage is committed with an independent receivable in the same transaction', async () => {
  const repository = await read('internal/billing/postgres.go');
  assert.match(repository, /backedActual := min\(actual, hold\.MaxMicrocredits\)/);
  assert.match(repository, /overage := actual - backedActual/);
  assert.match(repository, /createReceivableWithinTransaction\(ctx, tx, hold\.WalletID, "USAGE_OVERAGE"/);
  assert.doesNotMatch(repository, /if actual > hold\.MaxMicrocredits \{\s*return Hold\{\}, ErrConflict/);
});

test('only cash BASE settles oldest receivables while BONUS remains an entitlement', async () => {
  const payments = await read('internal/billing/payments.go');
  const receivables = await read('internal/billing/receivables.go');
  assert.match(payments, /component\.kind == "BASE"/);
  assert.match(payments, /settleOpenReceivablesWithinTransaction\(ctx, tx, order\.WalletID, baseAmount, "CASH_RECHARGE_BASE"/);
  assert.match(receivables, /ORDER BY created_at,id FOR UPDATE/);
  assert.match(receivables, /financial_status=CASE WHEN receivable_microcredits=\$2 THEN 'CLEAR'/);
});

test('confirmed chargeback targets evidence transaction package and creates only BASE shortfall debt', async () => {
  const chargebacks = await read('internal/billing/chargebacks.go');
  assert.match(chargebacks, /package\.source_payment_transaction_id=transaction\.id/);
  assert.match(chargebacks, /providerStatus != "CHARGEBACK_CONFIRMED"/);
  assert.match(chargebacks, /evidence_type,provider_id,provider_version,evidence_key/);
  assert.match(chargebacks, /'CHARGEBACK_CONFIRMED'/);
  assert.match(chargebacks, /if component\.kind == "BASE"/);
  assert.match(chargebacks, /item\.original, err = effectiveComponentOriginal\(historicalOriginal, reclassifiedOut\)/);
  assert.match(chargebacks, /baseShortfall \+= component\.original - revocable/);
  assert.match(chargebacks, /createReceivableWithinTransaction\(ctx, tx, walletID, "CHARGEBACK"/);
});

test('receivable allocations are immutable and expose only approved settlement source classes', async () => {
  const migration = await read('migrations/billing/000025_receivables_and_context_freeze.up.sql');
  assert.match(migration, /CASH_RECHARGE_BASE','REFUND_RECOVERY','APPROVED_MANUAL_REPAYMENT','APPROVED_WRITE_OFF/);
  assert.match(migration, /receivable_allocations_immutable/);
  assert.match(migration, /CHECK \(settled_microcredits \+ outstanding_microcredits = original_microcredits\)/);
});

test('operations contracts expose receivables allocations and confirmed chargeback boundary', async () => {
  const admin = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const billing = YAML.parse(await read('contracts/openapi/billing.yaml'));
  assert.ok(admin.paths['/billing/admin/v1/receivables'].get);
  assert.ok(admin.paths['/billing/admin/v1/receivables/{receivable_id}/allocations'].get);
  assert.ok(admin.components.schemas.Receivable.properties.outstanding_microcredits);
  assert.ok(billing.paths['/internal/v1/chargebacks/confirmed'].post);
  assert.ok(billing.components.schemas.WalletBalance.properties.receivable_microcredits);
  assert.ok(billing.components.schemas.ChargebackCase.properties.entitlement_package_id);
});
